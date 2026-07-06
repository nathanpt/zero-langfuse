package capture

import (
	"strings"
	"testing"

	"github.com/nathanpt/zero-langfuse/internal/redact"
)

func sample() Payload {
	return Payload{
		Input:        "user says hello",
		Output:       "assistant reply",
		ToolInput:    map[string]any{"path": "/x"},
		ToolOutput:   "tool result",
		SystemPrompt: "system instructions",
		Metadata:     map[string]any{"cwd": "/home/bob/proj", "title": "demo"},
	}
}

func TestPresetMetadataOnlyDropsAll(t *testing.T) {
	pol := FromEnv(nil, MetadataOnly)
	out := sample().Apply(pol, redact.DefaultRedactOptions)
	if out.Input != nil || out.Output != nil || out.ToolInput != nil || out.ToolOutput != nil || out.SystemPrompt != nil {
		t.Errorf("metadata-only must drop all content fields: %+v", out)
	}
	// metadata survives but cwd is gated off.
	if out.Metadata == nil {
		t.Fatal("metadata should survive (minus cwd)")
	}
	if _, has := out.Metadata["cwd"]; has {
		t.Errorf("cwd must be dropped under metadata-only")
	}
}

func TestPresetPromptsOnlyKeepsInput(t *testing.T) {
	pol := FromEnv(nil, PromptsOnly)
	out := sample().Apply(pol, redact.DefaultRedactOptions)
	if out.Input == nil {
		t.Error("prompts-only must keep Input")
	}
	if out.Output != nil || out.ToolInput != nil || out.SystemPrompt != nil {
		t.Errorf("prompts-only must drop Output/Tool/SystemPrompt: %+v", out)
	}
}

func TestPresetConversationsKeepsInputOutput(t *testing.T) {
	pol := FromEnv(nil, Conversations)
	out := sample().Apply(pol, redact.DefaultRedactOptions)
	if out.Input == nil || out.Output == nil {
		t.Error("conversations must keep Input+Output")
	}
	if out.ToolInput != nil || out.SystemPrompt != nil {
		t.Errorf("conversations must drop Tool/SystemPrompt: %+v", out)
	}
}

func TestPresetFullDebugKeepsAll(t *testing.T) {
	pol := FromEnv(nil, FullDebug)
	out := sample().Apply(pol, redact.DefaultRedactOptions)
	if out.Input == nil || out.Output == nil || out.ToolInput == nil || out.ToolOutput == nil || out.SystemPrompt == nil {
		t.Errorf("full-debug must keep all: %+v", out)
	}
	if _, has := out.Metadata["cwd"]; !has {
		t.Error("full-debug must keep cwd")
	}
}

// Default is full-debug.
func TestDefaultPresetIsFullDebug(t *testing.T) {
	pol := FromEnv(nil, "")
	if !pol.ToolIO || !pol.SystemPrompt || !pol.Cwd {
		t.Errorf("empty/unknown preset should default to full-debug: %+v", pol)
	}
	pol2 := FromEnv(nil, "nonsense")
	if pol2 != FromEnv(nil, FullDebug) {
		t.Errorf("unknown preset should fall back to full-debug: %+v", pol2)
	}
}

// LANGFUSE_CAPTURE_OUTPUTS=0 under conversations flips Outputs off.
func TestEnvFlagFlipsPresetField(t *testing.T) {
	env := map[string]string{"LANGFUSE_CAPTURE_OUTPUTS": "0"}
	pol := FromEnv(env, Conversations)
	if pol.Outputs {
		t.Error("LANGFUSE_CAPTURE_OUTPUTS=0 should disable Outputs")
	}
	if !pol.Inputs {
		t.Error("Inputs should still be on (from conversations)")
	}
}

func TestEnvPresetOverridesFile(t *testing.T) {
	env := map[string]string{"LANGFUSE_PRIVACY_PRESET": "metadata-only"}
	pol := FromEnv(env, FullDebug)
	if pol.Inputs || pol.Outputs {
		t.Errorf("env preset should override file preset: %+v", pol)
	}
}

// A secret in a retained Input is redacted.
func TestRetainedSecretIsRedacted(t *testing.T) {
	pol := FromEnv(nil, Conversations)
	p := Payload{Input: "token Bearer abcdefghijklmnop1234"}
	out := p.Apply(pol, redact.DefaultRedactOptions)
	s := out.Input.(string)
	if strings.Contains(s, "abcdefghijklmnop1234") {
		t.Errorf("secret leaked: %q", s)
	}
	if !strings.Contains(s, redact.Redacted) {
		t.Errorf("secret not masked: %q", s)
	}
}

// cwd gated by the Cwd flag only (independent of preset via env flag).
func TestCwdGatedIndependently(t *testing.T) {
	// conversations + LANGFUSE_CAPTURE_CWD=1 → cwd kept.
	env := map[string]string{"LANGFUSE_CAPTURE_CWD": "1"}
	pol := FromEnv(env, Conversations)
	out := sample().Apply(pol, redact.DefaultRedactOptions)
	if _, has := out.Metadata["cwd"]; !has {
		t.Error("LANGFUSE_CAPTURE_CWD=1 should keep cwd")
	}

	// full-debug + LANGFUSE_CAPTURE_CWD=0 → cwd dropped.
	env2 := map[string]string{"LANGFUSE_CAPTURE_CWD": "0"}
	pol2 := FromEnv(env2, FullDebug)
	out2 := sample().Apply(pol2, redact.DefaultRedactOptions)
	if _, has := out2.Metadata["cwd"]; has {
		t.Error("LANGFUSE_CAPTURE_CWD=0 should drop cwd")
	}
}
