// Package session reads Zero's persisted session log — the append-only
// events.jsonl plus metadata.json that Zero writes for every run surface (TUI,
// `zero exec`, and specialist sub-agents). It is the single source of truth for
// zero-langfuse (DESIGN §4).
//
// The event schema below is confirmed against the Zero source
// (internal/sessions/store.go, internal/usage/report.go, internal/cli/exec.go,
// internal/tui/model.go) and settles DESIGN §13:
//
//   - Q1 (trace segmentation): both exec and the TUI append a user `message`
//     event ({"role":"user","content":<prompt>}) before each turn's assistant
//     work, so "one trace per user turn" holds for both surfaces.
//   - Q2 (cache semantics): persisted `promptTokens` = EffectiveInputTokens() =
//     the TOTAL input (uncached + cache-read + cache-write). Cost must therefore
//     subtract BOTH cachedInputTokens and cacheWriteTokens from the input pool
//     (Zero's CalculateCost cost.go:91 does; DESIGN §7 corrected to match).
//   - Q3 (assistant-message completeness): the assistant `message` carries
//     result.FinalAnswer in a single event — one read, no delta reassembly.
//
// All three were re-confirmed against a live z.ai/glm-5.2 capture. Note
// (qualifies DESIGN §4): a plain-text `zero exec "prompt"` does NOT persist a
// session — recording is gated by shouldUseExecSession (exec.go:435) and only
// engages under --output-format stream-json|json, or --init-session-id/
// --resume/--fork. The TUI persists unconditionally. The trace/sync path must
// therefore expect text-mode exec runs to be invisible to a log reader.
//
// Live-confirmed payload shapes (z.ai/glm-5.2, relevant for Phase 1):
//   - tool_call.arguments is a JSON *string* (e.g. `{"path":"x"}`), not an
//     object — tool-input extraction must json.Unmarshal it again.
//   - tool_result carries {name, output, status, toolCallId, meta}; isError is
//     status != "ok" (DESIGN §6.2). meta holds byte/token estimates.
//   - A single user turn can span multiple provider_usage events (one generation
//     per model call) plus interleaved tool_call/tool_result pairs.
//
// Phase 0 scope: this package loads a session and pretty-prints every event.
// Payloads are dumped verbatim (not interpreted into traces) — interpretation,
// segmentation, and cost are Phase 1.
package session

import "encoding/json"

// Known event types written by Zero's session store (store.go). Mirrors the
// upstream sessions.EventType constants; kept here so the reader/dump do not
// import Zero's internal package.
const (
	EventMessage            = "message"
	EventToolCall           = "tool_call"
	EventToolResult         = "tool_result"
	EventPermission         = "permission"
	EventPermissionRequest  = "permission_request"
	EventPermissionDecision = "permission_decision"
	EventProviderUsage      = "provider_usage"
	EventError              = "error"
	EventSessionCheckpoint  = "session_checkpoint"
	EventSessionRewind      = "session_rewind"
	EventSessionCompaction  = "session_compaction"
	EventSessionFork        = "session_fork"
	EventSessionChild       = "session_child"
	EventSpecialistStart    = "specialist_start"
	EventSpecialistStop     = "specialist_stop"
	EventSpecDraft          = "spec_draft"
	EventSpecApproved       = "spec_approved"
	EventSpecRejected       = "spec_rejected"
)

// Event is one line of events.jsonl, mapped to Zero's sessions.Event envelope
// (store.go:173):
//
//	type Event struct {
//	  ID, SessionID string; Sequence int; Type EventType
//	  CreatedAt string; Payload json.RawMessage
//	}
//
// Raw is the original line bytes; Payload is extracted for display. A line that
// is not a JSON object is treated as torn (see parseEvent).
type Event struct {
	// Line is the 1-based source line in events.jsonl.
	Line int
	// ID is the event id (store-assigned).
	ID string
	// SessionID is the owning session id.
	SessionID string
	// Sequence is the 1-based event sequence within the session.
	Sequence int
	// Type is the event type ("message", "provider_usage", …).
	Type string
	// CreatedAt is the event timestamp (RFC3339, key "createdAt").
	CreatedAt string
	// Payload is the event body (key "payload"); nil when absent.
	Payload json.RawMessage
	// Raw is the original JSONL line bytes.
	Raw json.RawMessage
}

// rawEvent mirrors the on-disk envelope for JSON decoding.
type rawEvent struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionId"`
	Sequence  int             `json:"sequence"`
	Type      string          `json:"type"`
	CreatedAt string          `json:"createdAt"`
	Payload   json.RawMessage `json:"payload"`
}

// parseEvent parses one JSONL line into an Event. It returns nil for malformed
// or non-object lines; callers record those as torn (DESIGN §6.3: never abort
// the trace on a torn trailing line — Zero's own reader ignores them).
func parseEvent(line int, b []byte) *Event {
	var r rawEvent
	// A bare scalar/array/string fails to unmarshal into the struct and is
	// treated as torn, matching Zero's own lastEventSequence robustness.
	if err := json.Unmarshal(b, &r); err != nil {
		return nil
	}
	return &Event{
		Line:      line,
		ID:        r.ID,
		SessionID: r.SessionID,
		Sequence:  r.Sequence,
		Type:      r.Type,
		CreatedAt: r.CreatedAt,
		Payload:   r.Payload,
		Raw:       append([]byte(nil), b...),
	}
}

// summaryPeek returns a compact one-line description of an event's payload for
// the dump header, by best-effort peeking a few well-known fields per type. It
// never fails; unknown shapes return "" (the full payload is still dumped).
func (e *Event) summaryPeek() string {
	if len(e.Payload) == 0 {
		return ""
	}
	var p map[string]any
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return ""
	}
	switch e.Type {
	case EventMessage:
		role := asString(p["role"])
		if role != "" {
			return "role=" + role
		}
	case EventProviderUsage:
		return usageSummary(p)
	case EventToolCall:
		if n := asString(p["name"]); n != "" {
			return "tool=" + n
		}
		if n := asString(p["tool"]); n != "" {
			return "tool=" + n
		}
	case EventToolResult:
		if s := asString(p["status"]); s != "" {
			return "status=" + s
		}
	case EventError:
		if m := asString(p["message"]); m != "" {
			return trunc(m, 60)
		}
	}
	return ""
}

// usageSummary renders provider_usage token counts inline (promptTokens is the
// TOTAL input incl. cache, per Q2 — shown split so cache is visible at a glance).
func usageSummary(p map[string]any) string {
	in := asInt(p["promptTokens"])
	out := asInt(p["completionTokens"])
	s := "in=" + itoa(in) + " out=" + itoa(out)
	if c := asInt(p["cachedInputTokens"]); c > 0 {
		s += " cacheRead=" + itoa(c)
	}
	if c := asInt(p["cacheWriteTokens"]); c > 0 {
		s += " cacheWrite=" + itoa(c)
	}
	if c := asInt(p["reasoningTokens"]); c > 0 {
		s += " reasoning=" + itoa(c)
	}
	if m := asString(p["model"]); m != "" {
		s += " model=" + m
	}
	return s
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func itoa(n int) string {
	// Avoid a strconv import just for this display helper.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
