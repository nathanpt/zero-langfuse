// Package session reads Zero's persisted session log — the append-only
// events.jsonl plus metadata.json that Zero writes for every run surface (TUI,
// `zero exec`, and specialist sub-agents). It is the single source of truth for
// zero-langfuse (DESIGN §4).
//
// Phase 0 scope: this package loads and pretty-prints a session. The event
// envelope field names are settled only after a live capture (DESIGN §13), so
// parsing is deliberately envelope-agnostic: type/sequence/timestamp are
// extracted by best-effort over common keys and the full raw object is retained
// so `dump` reveals the real shape — which is the explicit purpose of Phase 0.
package session

import (
	"encoding/json"
	"strconv"
)

// Known event types written by Zero's session store (DESIGN §4.1). These are
// the only facts about the schema DESIGN source-verifies; the payload shapes
// for most are resolved at Phase 0.
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
)

// Event is one line of events.jsonl. The envelope field names are confirmed by
// the Phase 0 capture, so Type/Sequence/Timestamp are best-effort extractions;
// Raw and Object preserve everything for display and later typed access.
type Event struct {
	// Line is the 1-based source line in events.jsonl.
	Line int
	// Type is the best-effort event type (key "type", then "event"/"eventType").
	Type string
	// Sequence is the best-effort sequence number (key "sequence", then "seq").
	Sequence int
	// Timestamp is the best-effort raw timestamp string (key "timestamp", then
	// "time"/"ts"/"at").
	Timestamp string
	// Raw is the original line bytes (re-pretty-printed on dump).
	Raw json.RawMessage
	// Object is the parsed object for typed access in later phases.
	Object map[string]any
}

// parseEvent parses one JSONL line. It returns nil for malformed/torn lines;
// callers record those as skipped (DESIGN §6.3: never abort the trace on a torn
// trailing line — Zero's own reader ignores them).
func parseEvent(line int, raw []byte) *Event {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	// A valid JSON value that is not an object (e.g. a bare number) is not a
	// session event; treat as torn.
	if obj == nil {
		return nil
	}
	return &Event{
		Line:      line,
		Type:      pickStr(obj, "type", "event", "eventType"),
		Sequence:  pickInt(obj, "sequence", "seq"),
		Timestamp: pickStr(obj, "timestamp", "time", "ts", "at"),
		Raw:       append([]byte(nil), raw...),
		Object:    obj,
	}
}

// pickStr returns the first present string-valued key among candidates.
func pickStr(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// pickInt returns the first present integer-valued key among candidates. JSON
// numbers unmarshal as float64, so both float64 and string digits are handled.
func pickInt(obj map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			switch n := v.(type) {
			case float64:
				return int(n)
			case int:
				return n
			case string:
				if i, err := strconv.Atoi(n); err == nil {
					return i
				}
			}
		}
	}
	return 0
}
