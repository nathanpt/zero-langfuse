package trace

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nathanpt/zero-langfuse/internal/capture"
	"github.com/nathanpt/zero-langfuse/internal/langfuse"
	"github.com/nathanpt/zero-langfuse/internal/pricing"
	"github.com/nathanpt/zero-langfuse/internal/redact"
	"github.com/nathanpt/zero-langfuse/internal/session"
)

func ev(seq int, typ, payload string) *session.Event {
	return &session.Event{
		ID:        fmt.Sprintf("e%d", seq),
		SessionID: "s1",
		Sequence:  seq,
		Type:      typ,
		CreatedAt: fmt.Sprintf("2026-07-06T02:00:%02dZ", seq),
		Payload:   json.RawMessage(payload),
	}
}

// oneTurnSession: user msg → 2 usage + 1 tool_call/result + assistant msg.
func oneTurnSession(toolStatus, userContent string) *session.Session {
	return &session.Session{
		ID: "s1",
		Metadata: map[string]any{
			"sessionId":     "s1",
			"modelId":       "glm-5.2",
			"provider":      "zai",
			"cwd":           "/home/bob/proj",
			"title":         "demo",
			"rootSessionId": "s1",
		},
		Events: []*session.Event{
			ev(1, session.EventMessage, fmt.Sprintf(`{"role":"user","content":%q}`, userContent)),
			ev(2, session.EventProviderUsage, `{"promptTokens":120,"completionTokens":45,"totalTokens":165,"cachedInputTokens":80,"cacheWriteTokens":10}`),
			ev(3, session.EventToolCall, `{"id":"tc_1","name":"read_file","arguments":"{\"path\":\"/home/bob/secret.txt\"}"}`),
			ev(4, session.EventToolResult, fmt.Sprintf(`{"toolCallId":"tc_1","name":"read_file","status":%q,"output":"file contents"}`, toolStatus)),
			ev(5, session.EventProviderUsage, `{"promptTokens":200,"completionTokens":60,"totalTokens":260,"cachedInputTokens":150}`),
			ev(6, session.EventMessage, `{"role":"assistant","content":"done"}`),
		},
	}
}

func countByType(evs []langfuse.Event) map[string]int {
	m := map[string]int{}
	for _, e := range evs {
		m[e.Type]++
	}
	return m
}

func findByType(evs []langfuse.Event, typ string) []langfuse.Event {
	var out []langfuse.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestBuildEventCounts(t *testing.T) {
	s := oneTurnSession("ok", "hello")
	turns := Segment(s)
	if len(turns) != 1 {
		t.Fatalf("Segment → %d turns, want 1", len(turns))
	}
	evs := Build(turns[0], s.Metadata, capture.FromEnv(nil, capture.FullDebug), nil)
	got := countByType(evs)
	want := map[string]int{
		"trace-create":      1,
		"generation-create": 2,
		"span-create":       1,
		"score-create":      6,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %d, want %d (full: %v)", k, got[k], w, got)
		}
	}
}

func TestBuildToolIsError(t *testing.T) {
	s := oneTurnSession("error", "hello")
	evs := Build(Segment(s)[0], s.Metadata, capture.FromEnv(nil, capture.FullDebug), nil)
	spans := findByType(evs, "span-create")
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	body := spans[0].Body.(langfuse.ObsBody)
	if !body.Metadata["isError"].(bool) {
		t.Errorf("isError = false, want true for status=error")
	}
	// total_tool_errors score reflects it.
	for _, e := range findByType(evs, "score-create") {
		sb := e.Body.(langfuse.ScoreBody)
		if sb.Name == "total_tool_errors" && sb.Value != 1 {
			t.Errorf("total_tool_errors = %v, want 1", sb.Value)
		}
		if sb.Name == "turn_had_errors" && sb.Value != 1 {
			t.Errorf("turn_had_errors = %v, want 1", sb.Value)
		}
	}
}

func TestBuildToolOkNotError(t *testing.T) {
	s := oneTurnSession("ok", "hello")
	evs := Build(Segment(s)[0], s.Metadata, capture.FromEnv(nil, capture.FullDebug), nil)
	body := findByType(evs, "span-create")[0].Body.(langfuse.ObsBody)
	if body.Metadata["isError"].(bool) {
		t.Errorf("isError = true, want false for status=ok")
	}
}

// Generation costDetails.total must equal pricing.ComputeCost on the usage tokens.
func TestBuildGenerationCostMatchesComputeCost(t *testing.T) {
	s := oneTurnSession("ok", "hello")
	evs := Build(Segment(s)[0], s.Metadata, capture.FromEnv(nil, capture.FullDebug), nil)
	gens := findByType(evs, "generation-create")
	if len(gens) != 2 {
		t.Fatalf("generations = %d, want 2", len(gens))
	}
	// First usage: promptTokens 120, completionTokens 45, cachedInput 80, cacheWrite 10.
	price, _ := pricing.ResolvePrice("glm-5.2", nil)
	wantCost := pricing.ComputeCost(pricing.UsageTokens{Input: 120, CachedInput: 80, CacheWrite: 10, Output: 45}, price)
	body := gens[0].Body.(langfuse.ObsBody)
	if body.CostDetails == nil {
		t.Fatal("costDetails missing")
	}
	if got := body.CostDetails["total"]; got != wantCost.Total {
		t.Errorf("costDetails.total = %.10f, want %.10f", got, wantCost.Total)
	}
	// usageDetails present with cache fields.
	if body.UsageDetails["input"] != 120 || body.UsageDetails["cachedInput"] != 80 || body.UsageDetails["cacheWrite"] != 10 {
		t.Errorf("usageDetails = %v", body.UsageDetails)
	}
}

// metadata-only preset empties trace input/output.
func TestBuildMetadataOnlyDropsContent(t *testing.T) {
	s := oneTurnSession("ok", "hello")
	evs := Build(Segment(s)[0], s.Metadata, capture.FromEnv(nil, capture.MetadataOnly), nil)
	tb := findByType(evs, "trace-create")[0].Body.(langfuse.TraceBody)
	if tb.Input != nil {
		t.Errorf("metadata-only: trace Input = %v, want nil", tb.Input)
	}
	if tb.Output != nil {
		t.Errorf("metadata-only: trace Output = %v, want nil", tb.Output)
	}
	// cwd gated off under metadata-only.
	if _, has := tb.Metadata["cwd"]; has {
		t.Error("metadata-only: cwd should be dropped")
	}
}

// A secret in retained user content is redacted (full-debug keeps Input).
func TestBuildRedactsSecretInUserContent(t *testing.T) {
	s := oneTurnSession("ok", "my token is Bearer abcdefghijklmnop1234")
	evs := Build(Segment(s)[0], s.Metadata, capture.FromEnv(nil, capture.Conversations), nil)
	tb := findByType(evs, "trace-create")[0].Body.(langfuse.TraceBody)
	in, ok := tb.Input.(string)
	if !ok {
		t.Fatalf("Input not retained under conversations: %#v", tb.Input)
	}
	if strings.Contains(in, "abcdefghijklmnop1234") {
		t.Errorf("bearer leaked into trace input: %q", in)
	}
	if !strings.Contains(in, redact.Redacted) {
		t.Errorf("bearer not masked: %q", in)
	}
}

// Tool input (parsed args) is redacted: /home/bob path hashed.
func TestBuildToolInputRedacted(t *testing.T) {
	s := oneTurnSession("ok", "hello")
	evs := Build(Segment(s)[0], s.Metadata, capture.FromEnv(nil, capture.FullDebug), nil)
	body := findByType(evs, "span-create")[0].Body.(langfuse.ObsBody)
	if body.Input == nil {
		t.Fatal("tool input not retained under full-debug")
	}
	args, ok := body.Input.(map[string]any)
	if !ok {
		t.Fatalf("tool input not parsed to object: %#v", body.Input)
	}
	if p, ok := args["path"].(string); ok && strings.Contains(p, "/home/bob") {
		t.Errorf("absolute path leaked in tool args: %q", p)
	}
}

// Second fixture: orphan tool_result (no matching tool_call) + a turn with no
// usage → no generation, cache_hit_rate score present and 0.
func TestBuildOrphanToolResultAndNoUsage(t *testing.T) {
	s := &session.Session{
		ID:       "s2",
		Metadata: map[string]any{"modelId": "glm-5.2", "provider": "zai"},
		Events: []*session.Event{
			ev(1, session.EventMessage, `{"role":"user","content":"hi"}`),
			ev(2, session.EventToolResult, `{"toolCallId":"orphan_tc","name":"bash","status":"ok","output":"ok"}`),
		},
	}
	turns := Segment(s)
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	evs := Build(turns[0], s.Metadata, capture.FromEnv(nil, capture.FullDebug), nil)
	got := countByType(evs)
	if got["generation-create"] != 0 {
		t.Errorf("generation-create = %d, want 0 (no usage)", got["generation-create"])
	}
	if got["span-create"] != 1 {
		t.Errorf("span-create = %d, want 1 (orphan result)", got["span-create"])
	}
	// cache_hit_rate present and 0.
	var cacheRate *float64
	for _, e := range findByType(evs, "score-create") {
		sb := e.Body.(langfuse.ScoreBody)
		if sb.Name == "cache_hit_rate" {
			v := sb.Value
			cacheRate = &v
		}
	}
	if cacheRate == nil {
		t.Fatal("cache_hit_rate score missing")
	}
	if *cacheRate != 0 {
		t.Errorf("cache_hit_rate = %v, want 0 (no usage)", *cacheRate)
	}
}

// Events before the first user message form an implicit turn (openSeq=0).
func TestSegmentImplicitTurnForOrphanEvents(t *testing.T) {
	s := &session.Session{
		ID:       "s3",
		Metadata: map[string]any{"modelId": "glm-5.2"},
		Events: []*session.Event{
			ev(1, session.EventProviderUsage, `{"promptTokens":10,"completionTokens":5,"totalTokens":15}`),
			ev(2, session.EventMessage, `{"role":"user","content":"now a real turn"}`),
		},
	}
	turns := Segment(s)
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (implicit + real)", len(turns))
	}
	if turns[0].OpenSeq != 0 {
		t.Errorf("implicit turn OpenSeq = %d, want 0", turns[0].OpenSeq)
	}
	if turns[1].OpenSeq != 2 {
		t.Errorf("real turn OpenSeq = %d, want 2", turns[1].OpenSeq)
	}
}
