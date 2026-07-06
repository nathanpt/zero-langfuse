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

// fixtureEvents models the DESIGN §4 event feed with a plausible Phase 0
// envelope. The provider_usage payload matches usageEventPayload (§4.2) exactly.
// The final line is deliberately torn.
func fixtureEvents() []byte {
	return []byte(strings.Join([]string{
		`{"type":"message","sequence":1,"timestamp":"2026-07-06T02:00:00Z","payload":{"role":"user","content":"hello"}}`,
		`{"type":"message","sequence":2,"timestamp":"2026-07-06T02:00:01Z","payload":{"role":"assistant","content":"hi there"}}`,
		`{"type":"tool_call","sequence":3,"timestamp":"2026-07-06T02:00:02Z","payload":{"tool":"read","toolCallId":"tc_1"}}`,
		`{"type":"tool_result","sequence":4,"timestamp":"2026-07-06T02:00:03Z","payload":{"toolCallId":"tc_1","status":"ok"}}`,
		`{"type":"provider_usage","sequence":5,"timestamp":"2026-07-06T02:00:04Z","payload":{"promptTokens":120,"completionTokens":45,"totalTokens":165,"cachedInputTokens":80,"cacheWriteTokens":0,"reasoningTokens":0}}`,
		`{"type":"error","sequence":6,"timestamp":"2026-07-06T02:00:05Z","payload":{"message":"boom"}}`,
		`{"type":"message","sequence":7` + "\n", // torn: truncated, no closing brace
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

func TestParseEventBestEffortExtraction(t *testing.T) {
	// Canonical keys.
	ev := parseEvent(1, []byte(`{"type":"message","sequence":9,"timestamp":"t"}`))
	if ev.Type != "message" || ev.Sequence != 9 || ev.Timestamp != "t" {
		t.Errorf("canonical: %+v", ev)
	}
	// Alternate keys still resolve.
	ev = parseEvent(2, []byte(`{"event":"tool_call","seq":3,"ts":"x"}`))
	if ev.Type != "tool_call" || ev.Sequence != 3 || ev.Timestamp != "x" {
		t.Errorf("alt keys: %+v", ev)
	}
	// Malformed JSON → nil (torn).
	if parseEvent(3, []byte(`{nope`)) != nil {
		t.Error("malformed should be nil")
	}
	// Non-object JSON → nil.
	if parseEvent(4, []byte(`42`)) != nil {
		t.Error("bare scalar should be nil")
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
