// Package trace turns a loaded session into Langfuse ingestion events: it
// segments the event stream into one Turn per user message (DESIGN §6.1) and
// builds a trace-create + generations + tool spans + scores batch per turn
// (DESIGN §6.2).
//
// Segmentation: a user `message` opens a Turn; everything until the next user
// message (or end-of-session) attaches to it. Events before the first user
// message form an implicit turn (openSeq=0) so orphans are not dropped
// (DESIGN §6.3 self-close).
//
// All content fields flow through capture.Apply (privacy preset + redaction)
// before emission. Every event id is deterministic so re-runs are idempotent.
package trace

import (
	"encoding/json"
	"sort"

	"github.com/nathanpt/zero-langfuse/internal/capture"
	"github.com/nathanpt/zero-langfuse/internal/langfuse"
	"github.com/nathanpt/zero-langfuse/internal/pricing"
	"github.com/nathanpt/zero-langfuse/internal/redact"
	"github.com/nathanpt/zero-langfuse/internal/session"
)

// maxToolOutputBytes bounds a tool_result output string before redaction.
const toolOutputLimit = 24000

// Turn is one user turn: the opener message and every event that attaches to
// it until the next user message. Built by Segment; consumed by Build.
type Turn struct {
	SessionID    string
	OpenSeq      int            // opener message's sequence (0 if implicit)
	Opener       *session.Event // the user message (nil for implicit)
	FirstEvent   *session.Event // first attached event (timestamp fallback)
	UsageEvents  []*session.Event
	ToolCalls    map[string]*session.Event // toolCallId → tool_call
	ToolResults  map[string]*session.Event // toolCallId → tool_result
	Errors       []*session.Event
	AssistantOut any // content of the last assistant message
}

// Segment splits a session's events into Turns (DESIGN §6.1).
func Segment(s *session.Session) []*Turn {
	var turns []*Turn
	var cur *Turn
	flush := func() {
		if cur != nil {
			turns = append(turns, cur)
			cur = nil
		}
	}
	for _, ev := range s.Events {
		if ev.Type == session.EventMessage && payloadRole(ev) == "user" {
			flush()
			cur = newTurn(s.ID, ev.Sequence, ev)
			continue
		}
		if cur == nil {
			// Implicit opener for events preceding any user message.
			cur = newTurn(s.ID, 0, nil)
		}
		if cur.FirstEvent == nil {
			cur.FirstEvent = ev
		}
		attach(cur, ev)
	}
	flush()
	return turns
}

func newTurn(sessionID string, openSeq int, opener *session.Event) *Turn {
	return &Turn{
		SessionID:   sessionID,
		OpenSeq:     openSeq,
		Opener:      opener,
		ToolCalls:   map[string]*session.Event{},
		ToolResults: map[string]*session.Event{},
	}
}

func attach(t *Turn, ev *session.Event) {
	switch ev.Type {
	case session.EventMessage:
		if payloadRole(ev) == "assistant" {
			t.AssistantOut = payloadContent(ev)
		}
	case session.EventProviderUsage:
		t.UsageEvents = append(t.UsageEvents, ev)
	case session.EventToolCall:
		if id := payloadToolCallID(ev); id != "" {
			t.ToolCalls[id] = ev
		}
	case session.EventToolResult:
		if id := payloadValueStr(ev, "toolCallId"); id != "" {
			t.ToolResults[id] = ev
		}
	case session.EventError:
		t.Errors = append(t.Errors, ev)
	}
}

// Build emits the Langfuse events for one Turn: a trace-create, one
// generation-create per provider_usage, one span-create per tool_call (matched
// to its tool_result), and six trace-level scores. All content is gated through
// the active privacy policy + redaction.
func Build(t *Turn, meta map[string]any, pol capture.Policy, priceOverrides map[string]pricing.TokenPrice) []langfuse.Event {
	ro := redact.DefaultRedactOptions
	var evs []langfuse.Event

	traceResID := langfuse.TraceID(t.SessionID, t.OpenSeq)
	ts := traceTimestamp(t)

	// Error level/status for the turn's observations (DESIGN §6.2).
	level, status := "", ""
	if len(t.Errors) > 0 {
		level = "ERROR"
		status = payloadValueStr(t.Errors[len(t.Errors)-1], "message")
	}

	// 1. trace-create
	traceMeta := buildTraceMeta(meta)
	if status != "" {
		// Record the turn's error on the trace (Langfuse trace bodies carry no
		// level field; the score turn_had_errors is the structured signal).
		if traceMeta == nil {
			traceMeta = map[string]any{}
		}
		traceMeta["error"] = status
	}
	userContent := any(nil)
	if t.Opener != nil {
		userContent = payloadContent(t.Opener)
	}
	tp := capture.Payload{Input: userContent, Output: t.AssistantOut, Metadata: traceMeta}.Apply(pol, ro)
	evs = append(evs, langfuse.Event{
		Type:      "trace-create",
		ID:        langfuse.EventID("trace-create", traceResID),
		Timestamp: ts,
		Body: langfuse.TraceBody{
			ID:        traceResID,
			Timestamp: ts,
			Name:      "zero-agent",
			Input:     tp.Input,
			Output:    tp.Output,
			SessionID: t.SessionID,
			Metadata:  tp.Metadata,
		},
	})

	// 2. generations (one per provider_usage)
	for _, ue := range t.UsageEvents {
		model := payloadValueStr(ue, "model")
		if model == "" {
			model = asString(meta["modelId"])
		}
		ud, usage := usageDetails(ue)
		body := langfuse.ObsBody{
			ID:            langfuse.GenerationID(t.SessionID, ue.Sequence),
			TraceID:       traceResID,
			Name:          "llm-generation",
			StartTime:     ue.CreatedAt,
			EndTime:       ue.CreatedAt,
			Model:         model,
			UsageDetails:  ud,
			Level:         level,
			StatusMessage: status,
		}
		if price, ok := pricing.ResolvePrice(model, priceOverrides); ok {
			body.CostDetails = costMap(pricing.ComputeCost(usage, price))
		} else {
			pricing.WarnOnceNoPrice(model)
		}
		evs = append(evs, langfuse.Event{
			Type:      "generation-create",
			ID:        langfuse.EventID("generation-create", body.ID),
			Timestamp: ue.CreatedAt,
			Body:      body,
		})
	}

	// 3. tool spans — deterministic order by tool_call sequence.
	type toolPair struct {
		callID    string
		call, res *session.Event
	}
	pairs := make([]toolPair, 0, len(t.ToolCalls))
	for callID, call := range t.ToolCalls {
		pairs = append(pairs, toolPair{callID, call, t.ToolResults[callID]})
	}
	for callID, res := range t.ToolResults {
		if _, has := t.ToolCalls[callID]; has {
			continue
		}
		pairs = append(pairs, toolPair{callID, nil, res}) // orphan result
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairSeq(pairs[i]) < pairSeq(pairs[j])
	})
	for _, pr := range pairs {
		evs = append(evs, buildToolSpan(t.SessionID, traceResID, pr.callID, pr.call, pr.res, pol, ro, level, status))
	}

	// 4. scores (DESIGN §6.2).
	toolCount := len(t.ToolCalls)
	toolErrors := 0
	for _, res := range t.ToolResults {
		if payloadValueStr(res, "status") != "ok" {
			toolErrors++
		}
	}
	hadErrors := len(t.Errors) > 0 || toolErrors > 0
	successRate := 1.0
	if toolCount > 0 {
		successRate = float64(toolCount-toolErrors) / float64(toolCount)
	}
	sumPrompt, sumCached := 0, 0
	for _, ue := range t.UsageEvents {
		sumPrompt += payloadInt(ue, "promptTokens")
		sumCached += payloadInt(ue, "cachedInputTokens")
	}
	cacheHitRate := 0.0
	if sumPrompt > 0 {
		cacheHitRate = float64(sumCached) / float64(sumPrompt)
	}
	hadErrorsN := 0.0
	if hadErrors {
		hadErrorsN = 1.0
	}
	scores := []struct {
		name  string
		value float64
	}{
		{"tool_call_count", float64(toolCount)},
		{"total_tool_errors", float64(toolErrors)},
		{"tool_success_rate", successRate},
		{"turn_had_errors", hadErrorsN},
		{"generation_count", float64(len(t.UsageEvents))},
		{"cache_hit_rate", cacheHitRate},
	}
	for _, sc := range scores {
		evs = append(evs, langfuse.Event{
			Type:      "score-create",
			ID:        langfuse.EventID("score-create", langfuse.ScoreID(traceResID, sc.name)),
			Timestamp: ts,
			Body: langfuse.ScoreBody{
				ID:       langfuse.ScoreID(traceResID, sc.name),
				TraceID:  traceResID,
				Name:     sc.name,
				Value:    sc.value,
				DataType: "NUMERIC", // Langfuse ingestion keys dataType on the UPPERCASE enum.
			},
		})
	}

	return evs
}

func buildToolSpan(sessionID, traceResID, callID string, call, res *session.Event, pol capture.Policy, ro redact.RedactOptions, level, status string) langfuse.Event {
	name := ""
	startTime := ""
	if call != nil {
		name = payloadValueStr(call, "name")
		startTime = call.CreatedAt
	}
	if name == "" && res != nil {
		name = payloadValueStr(res, "name")
	}
	endTime := ""
	if res != nil {
		endTime = res.CreatedAt
	}

	// Input: tool_call.arguments is a JSON *string* (live-confirmed); parse it.
	var toolInput any
	if call != nil {
		if argsStr := payloadValueStr(call, "arguments"); argsStr != "" {
			var parsed any
			if json.Unmarshal([]byte(argsStr), &parsed) == nil {
				toolInput = parsed
			} else {
				toolInput = argsStr
			}
		}
	}
	// Output: tool_result.output, string-truncated to toolOutputLimit.
	var toolOutput any
	if res != nil {
		toolOutput = payloadValue(res, "output")
		if s, ok := toolOutput.(string); ok && len(s) > toolOutputLimit {
			toolOutput = s[:toolOutputLimit] + "... [truncated]"
		}
	}
	p := capture.Payload{ToolInput: toolInput, ToolOutput: toolOutput}.Apply(pol, ro)

	spanMeta := map[string]any{"toolCallId": callID}
	if res != nil {
		st := payloadValueStr(res, "status")
		spanMeta["status"] = st
		spanMeta["isError"] = st != "ok" // DESIGN §6.2
	} else {
		spanMeta["status"] = "missing"
		spanMeta["isError"] = false
	}

	ts := startTime
	if ts == "" {
		ts = endTime
	}
	return langfuse.Event{
		Type:      "span-create",
		ID:        langfuse.EventID("span-create", langfuse.ToolID(sessionID, callID)),
		Timestamp: ts,
		Body: langfuse.ObsBody{
			ID:            langfuse.ToolID(sessionID, callID),
			TraceID:       traceResID,
			Name:          name,
			StartTime:     startTime,
			EndTime:       endTime,
			Input:         p.ToolInput,
			Output:        p.ToolOutput,
			Metadata:      spanMeta,
			Level:         level,
			StatusMessage: status,
		},
	}
}

func pairSeq(p struct {
	callID    string
	call, res *session.Event
}) int {
	if p.call != nil {
		return p.call.Sequence
	}
	if p.res != nil {
		return p.res.Sequence
	}
	return 0
}

func traceTimestamp(t *Turn) string {
	if t.Opener != nil && t.Opener.CreatedAt != "" {
		return t.Opener.CreatedAt
	}
	if t.FirstEvent != nil {
		return t.FirstEvent.CreatedAt
	}
	return ""
}

// buildTraceMeta copies the trace-relevant metadata fields from the session
// metadata. cwd is gated by capture.Apply (dropped when !pol.Cwd).
func buildTraceMeta(meta map[string]any) map[string]any {
	if meta == nil {
		return nil
	}
	keys := []string{"provider", "modelId", "cwd", "title", "rootSessionId"}
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := meta[k]; ok && v != nil {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func usageDetails(ev *session.Event) (map[string]int, pricing.UsageTokens) {
	p := payloadInt(ev, "promptTokens")
	c := payloadInt(ev, "completionTokens")
	tot := payloadInt(ev, "totalTokens")
	ci := payloadInt(ev, "cachedInputTokens")
	cw := payloadInt(ev, "cacheWriteTokens")
	rt := payloadInt(ev, "reasoningTokens")
	ud := map[string]int{
		"input":  p,
		"output": c,
		"total":  tot,
	}
	if ci > 0 {
		ud["cachedInput"] = ci
	}
	if cw > 0 {
		ud["cacheWrite"] = cw
	}
	if rt > 0 {
		ud["reasoning"] = rt
	}
	return ud, pricing.UsageTokens{Input: p, CachedInput: ci, CacheWrite: cw, Output: c}
}

func costMap(c pricing.CostBreakdown) map[string]float64 {
	// Mirror usageDetails: input/output/total always present; cache components
	// only when non-zero (keeps the payload lean and consistent).
	m := map[string]float64{
		"input":  c.Input,
		"output": c.Output,
		"total":  c.Total,
	}
	if c.CachedInput != 0 {
		m["cachedInput"] = c.CachedInput
	}
	if c.CacheWrite != 0 {
		m["cacheWrite"] = c.CacheWrite
	}
	return m
}

// ---- payload accessors (session payloads are json.RawMessage) ----

func payloadMap(ev *session.Event) map[string]any {
	if ev == nil || len(ev.Payload) == 0 {
		return nil
	}
	var p map[string]any
	_ = json.Unmarshal(ev.Payload, &p)
	return p
}

func payloadRole(ev *session.Event) string { return asString(payloadMap(ev)["role"]) }
func payloadContent(ev *session.Event) any { return payloadMap(ev)["content"] }
func payloadValue(ev *session.Event, k string) any {
	return payloadMap(ev)[k]
}
func payloadValueStr(ev *session.Event, k string) string {
	return asString(payloadMap(ev)[k])
}
func payloadInt(ev *session.Event, k string) int {
	return asInt(payloadMap(ev)[k])
}

// payloadToolCallID reads the tool_call id (payload.id; some shapes use "toolCallId").
func payloadToolCallID(ev *session.Event) string {
	if id := payloadValueStr(ev, "id"); id != "" {
		return id
	}
	return payloadValueStr(ev, "toolCallId")
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
