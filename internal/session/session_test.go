package session

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// writeSession creates a session dir under root with the given metadata and
// events.jsonl bytes. nil bytes omit that file.
func writeSession(t *testing.T, root, id string, metadata, events []byte) string {
	t.Helper()
	sd := filepath.Join(root, id)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	if metadata != nil {
		if err := os.WriteFile(filepath.Join(sd, metadataFile), metadata, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if events != nil {
		if err := os.WriteFile(filepath.Join(sd, eventsFile), events, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return sd
}

// fixtureEvents uses the real sessions.Event envelope
// (id/sessionId/sequence/type/createdAt/payload). The provider_usage payload
// mirrors usageEventPayload (usage/report.go:21): promptTokens is the TOTAL
// input (uncached 30 + cacheRead 80 + cacheWrite 10). The final line is torn.
func fixtureEvents() []byte {
	return []byte(strings.Join([]string{
		`{"id":"e1","sessionId":"sess-abc","sequence":1,"type":"message","createdAt":"2026-07-06T02:00:00Z","payload":{"role":"user","content":"hello"}}`,
		`{"id":"e2","sessionId":"sess-abc","sequence":2,"type":"message","createdAt":"2026-07-06T02:00:01Z","payload":{"role":"assistant","content":"hi there"}}`,
		`{"id":"e3","sessionId":"sess-abc","sequence":3,"type":"tool_call","createdAt":"2026-07-06T02:00:02Z","payload":{"id":"tc_1","name":"read_file"}}`,
		`{"id":"e4","sessionId":"sess-abc","sequence":4,"type":"tool_result","createdAt":"2026-07-06T02:00:03Z","payload":{"toolCallId":"tc_1","status":"ok"}}`,
		`{"id":"e5","sessionId":"sess-abc","sequence":5,"type":"provider_usage","createdAt":"2026-07-06T02:00:04Z","payload":{"promptTokens":120,"completionTokens":45,"totalTokens":165,"cachedInputTokens":80,"cacheWriteTokens":10,"reasoningTokens":0}}`,
		`{"id":"e6","sessionId":"sess-abc","sequence":6,"type":"error","createdAt":"2026-07-06T02:00:05Z","payload":{"message":"boom"}}`,
		`{"id":"e7","sessionId":"sess-abc","sequence":7` + "\n", // torn: truncated, no closing brace
	}, "\n"))
}

func fixtureMetadata() []byte {
	return []byte(`{"sessionId":"sess-abc","modelId":"glm-5.2","provider":"zai","cwd":"/tmp/proj","title":"demo","rootSessionId":"sess-abc"}`)
}

func TestLoadParsesEventsAndFlagsTorn(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "sess-abc", fixtureMetadata(), fixtureEvents())

	s, err := Load("sess-abc", root)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "sess-abc" {
		t.Errorf("ID = %q, want sess-abc", s.ID)
	}
	if s.Metadata["modelId"] != "glm-5.2" {
		t.Errorf("metadata not parsed: %#v", s.Metadata)
	}
	if got := len(s.Events); got != 6 {
		t.Fatalf("Events = %d, want 6", got)
	}
	if len(s.TornLines) != 1 || s.TornLines[0] != 7 {
		t.Errorf("TornLines = %v, want [7]", s.TornLines)
	}
}
func TestParseEventEnvelope(t *testing.T) {
	ev := parseEvent(1, []byte(`{"id":"e1","sessionId":"s","sequence":9,"type":"message","createdAt":"2026-07-06T02:00:00Z","payload":{"role":"user","content":"hi"}}`))
	if ev == nil {
		t.Fatal("nil event")
	}
	if ev.ID != "e1" || ev.SessionID != "s" || ev.Sequence != 9 || ev.Type != "message" || ev.CreatedAt != "2026-07-06T02:00:00Z" {
		t.Errorf("envelope fields wrong: %+v", ev)
	}
	if got := string(ev.Payload); got != `{"role":"user","content":"hi"}` {
		t.Errorf("payload = %q", got)
	}
	// payload is optional; the event still parses without it.
	if ev2 := parseEvent(2, []byte(`{"sequence":1,"type":"message","createdAt":"t"}`)); ev2 == nil || len(ev2.Payload) != 0 {
		t.Errorf("missing-payload event should parse: %+v", ev2)
	}
	// Malformed / non-object lines are torn (nil), never fatal.
	for i, bad := range []string{`{nope`, `42`, `true`, `[1,2,3]`, `"str"`} {
		if parseEvent(i, []byte(bad)) != nil {
			t.Errorf("non-event %q should be nil", bad)
		}
	}
}

func TestTornLineMidFileIsSkippedNotFatal(t *testing.T) {
	root := t.TempDir()
	events := []byte(strings.Join([]string{
		`{"type":"message","sequence":1}`,
		`{ this is not json `,
		`{"type":"provider_usage","sequence":2}`,
		"",
	}, "\n"))
	writeSession(t, root, "s", nil, events)

	s, err := Load("s", root)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Events) != 2 {
		t.Fatalf("Events = %d, want 2", len(s.Events))
	}
	if len(s.TornLines) != 1 || s.TornLines[0] != 2 {
		t.Errorf("TornLines = %v, want [2]", s.TornLines)
	}
}

func TestDumpRendersMetadataEventsAndSummary(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "sess-abc", fixtureMetadata(), fixtureEvents())

	s, err := Load("sess-abc", root)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.Dump(&buf, "", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"session: sess-abc",
		"metadata.json:",
		"\"modelId\": \"glm-5.2\"",
		"events: 6 parsed, 1 torn/skipped",
		"──[#1] message",
		"role=user",
		"role=assistant",
		"in=120 out=45 cacheRead=80 cacheWrite=10", // Q2: promptTokens(total) split into cache fields
		"──[#5] provider_usage",
		"\"promptTokens\": 120",
		"summary: 6 event(s)",
		fmt.Sprintf("  %-22s %d", "message", 2),
		fmt.Sprintf("  %-22s %d", "provider_usage", 1),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestDumpFilterByType(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "s", nil, fixtureEvents())
	s, err := Load("s", root)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.Dump(&buf, "provider_usage", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "provider_usage") {
		t.Error("missing usage event")
	}
	if strings.Contains(out, "──[#1] message") {
		t.Error("filter leaked non-matching event:\n" + out)
	}
	// Summary still counts all.
	if !strings.Contains(out, "summary: 6 event(s)") {
		t.Error("summary should reflect all events")
	}
}

func TestDumpSummaryOnly(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "s", nil, fixtureEvents())
	s, err := Load("s", root)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.Dump(&buf, "", true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "──[") {
		t.Error("summary-only should omit event bodies:\n" + out)
	}
	if !strings.Contains(out, "summary:") {
		t.Error("summary-only must include summary")
	}
}

func TestFindLatestPicksNewest(t *testing.T) {
	root := t.TempDir()
	older := writeSession(t, root, "older", fixtureMetadata(), []byte(`{"type":"message","sequence":1}` + "\n"))
	newer := writeSession(t, root, "newer", fixtureMetadata(), []byte(`{"type":"message","sequence":1}` + "\n"))

	base := time.Now()
	if err := os.Chtimes(filepath.Join(older, eventsFile), base.Add(-2*time.Hour), base.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(newer, eventsFile), base, base); err != nil {
		t.Fatal(err)
	}

	got, err := FindLatest(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != "newer" {
		t.Errorf("FindLatest = %q, want newer", got)
	}
}

func TestFindLatestEmptyDirErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := FindLatest(root); err == nil {
		t.Error("expected error on empty sessions dir")
	}
}

func TestResolveSessionDir(t *testing.T) {
	root := t.TempDir()
	dir := writeSession(t, root, "sess-x", fixtureMetadata(), []byte(`{"type":"message","sequence":1}` + "\n"))

	// By session id under sessionsDir.
	if got, err := resolveSessionDir("sess-x", root); err != nil || got != dir {
		t.Errorf("by id: got %q err %v", got, err)
	}
	// By directory path.
	if got, err := resolveSessionDir(dir, ""); err != nil || got != dir {
		t.Errorf("by dir: got %q err %v", got, err)
	}
	// By events.jsonl path (uses parent).
	evPath := filepath.Join(dir, eventsFile)
	if got, err := resolveSessionDir(evPath, ""); err != nil || got != dir {
		t.Errorf("by file: got %q err %v", got, err)
	}
	// Unknown id with no sessionsDir → error.
	if _, err := resolveSessionDir("nope", ""); err == nil {
		t.Error("expected error for unknown id without sessions dir")
	}
}

func TestDefaultSessionsDirHonorsXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/custom/xdg")
	got, err := DefaultSessionsDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/custom/xdg/zero/sessions"; got != want {
		t.Errorf("DefaultSessionsDir = %q, want %q", got, want)
	}
}

func TestDefaultSessionsDirFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DefaultSessionsDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "zero", "sessions"); got != want {
		t.Errorf("DefaultSessionsDir = %q, want %q", got, want)
	}
}

// TestSortedKeysIsStable is a no-op guard ensuring sort helpers compile; kept to
// document the summary ordering expectation (by count desc, then name asc).
func TestSummaryOrdering(t *testing.T) {
	counts := map[string]int{"a": 2, "b": 2, "c": 5}
	types := make([]string, 0, len(counts))
	for k := range counts {
		types = append(types, k)
	}
	sort.Slice(types, func(i, j int) bool {
		if counts[types[i]] != counts[types[j]] {
			return counts[types[i]] > counts[types[j]]
		}
		return types[i] < types[j]
	})
	want := []string{"c", "a", "b"}
	for i, w := range want {
		if types[i] != w {
			t.Errorf("order[%d] = %q, want %q (%v)", i, types[i], w, types)
		}
	}
}
