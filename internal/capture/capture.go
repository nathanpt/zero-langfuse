// Package capture applies the privacy preset + redaction to a telemetry
// payload before it is uploaded (DESIGN §8). It ports omp-langfuse's
// src/capture-policy.ts: the same four presets, the same fine-grained env
// flags, the same field-gating + redaction.
//
// The session log is un-redacted on disk (DESIGN §8.2); this package is the
// gate between that private transcript and anything that leaves the machine.
package capture

import (
	"regexp"

	"github.com/nathanpt/zero-langfuse/internal/redact"
)

// PrivacyPreset names a capture level. Values match omp's preset strings.
type PrivacyPreset string

const (
	MetadataOnly  PrivacyPreset = "metadata-only"
	PromptsOnly   PrivacyPreset = "prompts-only"
	Conversations PrivacyPreset = "conversations"
	FullDebug     PrivacyPreset = "full-debug"
	defaultPreset PrivacyPreset = FullDebug
)

// Policy is the resolved per-field capture decision. Mirrors omp CapturePolicy.
type Policy struct {
	Inputs       bool
	Outputs      bool
	ToolIO       bool
	SystemPrompt bool
	Cwd          bool
}

// presets ports capture-policy.ts:32-61.
var presets = map[PrivacyPreset]Policy{
	MetadataOnly:  {Inputs: false, Outputs: false, ToolIO: false, SystemPrompt: false, Cwd: false},
	PromptsOnly:   {Inputs: true, Outputs: false, ToolIO: false, SystemPrompt: false, Cwd: false},
	Conversations: {Inputs: true, Outputs: true, ToolIO: false, SystemPrompt: false, Cwd: false},
	FullDebug:     {Inputs: true, Outputs: true, ToolIO: true, SystemPrompt: true, Cwd: true},
}

var (
	flagTrueRE  = regexp.MustCompile(`(?i)^(1|true|yes|on)$`)
	flagFalseRE = regexp.MustCompile(`(?i)^(0|false|no|off)$`)
)

// parseFlag mirrors capture-policy.ts parseFlag: returns true/false for the
// recognized on/off spellings, or ok=false for anything else (no override).
func parseFlag(v string) (b bool, ok bool) {
	if flagTrueRE.MatchString(v) {
		return true, true
	}
	if flagFalseRE.MatchString(v) {
		return false, true
	}
	return false, false
}

// NormalizePreset returns the canonical preset for s, defaulting to full-debug
// (matching omp normalizePreset). Unknown values fall back to the default.
func NormalizePreset(s string) PrivacyPreset {
	p := PrivacyPreset(s)
	if _, has := presets[p]; has {
		return p
	}
	return defaultPreset
}

// FromEnv resolves a capture policy. It starts from the file preset, lets the
// env LANGFUSE_PRIVACY_PRESET override it, then applies the five fine-grained
// boolean flags. Ports createCapturePolicy (capture-policy.ts:88-99), adapted
// to take an explicit env map + file preset.
func FromEnv(env map[string]string, filePreset PrivacyPreset) Policy {
	preset := NormalizePreset(string(filePreset))
	if v, ok := env["LANGFUSE_PRIVACY_PRESET"]; ok && v != "" {
		preset = NormalizePreset(v)
	}
	pol := presets[preset]

	type flagBind struct {
		env   string
		field *bool
	}
	binds := []flagBind{
		{"LANGFUSE_CAPTURE_INPUTS", &pol.Inputs},
		{"LANGFUSE_CAPTURE_OUTPUTS", &pol.Outputs},
		{"LANGFUSE_CAPTURE_TOOL_IO", &pol.ToolIO},
		{"LANGFUSE_CAPTURE_SYSTEM_PROMPT", &pol.SystemPrompt},
		{"LANGFUSE_CAPTURE_CWD", &pol.Cwd},
	}
	for _, b := range binds {
		if v, ok := env[b.env]; ok {
			if val, ok := parseFlag(v); ok {
				*b.field = val
			}
		}
	}
	return pol
}

// Payload is the redactable telemetry for one observation, mirroring omp's
// RawTelemetryPayload. Disabled fields are left nil by Apply; callers marshal
// with omitempty so they drop out entirely.
type Payload struct {
	Input        any            `json:"input,omitempty"`
	Output       any            `json:"output,omitempty"`
	ToolInput    any            `json:"toolInput,omitempty"`
	ToolOutput   any            `json:"toolOutput,omitempty"`
	SystemPrompt any            `json:"systemPrompt,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// Apply drops fields the policy disables, redacts every retained field, and
// gates metadata.cwd behind the Cwd flag. Ports applyCapturePolicy
func (p Payload) Apply(pol Policy, ro redact.RedactOptions) Payload {
	out := Payload{}
	out.Metadata = redactMetadata(p.Metadata, pol, ro)
	if pol.Inputs {
		out.Input = redact.RedactValue(p.Input, ro)
	}
	if pol.Outputs {
		out.Output = redact.RedactValue(p.Output, ro)
	}
	if pol.ToolIO {
		out.ToolInput = redact.RedactValue(p.ToolInput, ro)
		out.ToolOutput = redact.RedactValue(p.ToolOutput, ro)
	}
	if pol.SystemPrompt {
		out.SystemPrompt = redact.RedactValue(p.SystemPrompt, ro)
	}
	return out
}

func redactMetadata(m map[string]any, pol Policy, ro redact.RedactOptions) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == "cwd" && !pol.Cwd {
			continue
		}
		out[k] = redact.RedactValue(v, ro)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
