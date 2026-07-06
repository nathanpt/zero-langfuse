package langfuse

import "testing"

// RFC 4122 Appendix A test vector: UUIDv5 over the DNS namespace with the
// name "python.org" must equal 886313e1-3b8a-5372-9b90-0c9aee199e5d.
func TestUUIDv5RFC4122Vector(t *testing.T) {
	got := MustUUID(dnsNamespace, "python.org")
	want := "886313e1-3b8a-5372-9b90-0c9aee199e5d"
	if got != want {
		t.Errorf("UUIDv5(python.org) = %q, want %q", got, want)
	}
}

func TestUUIDv5VersionAndVariant(t *testing.T) {
	u := uuidV5(dnsNamespace, "anything")
	if v := u[6] >> 4; v != 5 {
		t.Errorf("version nibble = %d, want 5", v)
	}
	if v := u[8] >> 6; v != 2 { // top two bits = 10
		t.Errorf("variant bits = %02b, want 10", v)
	}
}

func TestUUIDv5Deterministic(t *testing.T) {
	a := MustUUID(namespace, "trace:sess-abc:1")
	b := MustUUID(namespace, "trace:sess-abc:1")
	if a != b {
		t.Errorf("non-deterministic: %q vs %q", a, b)
	}
	// Different name → different id.
	c := MustUUID(namespace, "trace:sess-abc:2")
	if a == c {
		t.Errorf("distinct names produced identical ids")
	}
}

func TestTraceIDShape(t *testing.T) {
	id := TraceID("sess-abc", 1)
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Errorf("TraceID not UUID-shaped: %q", id)
	}
}

func TestGenerationToolScoreIDs(t *testing.T) {
	if g := GenerationID("s", 3); g != "gen:s:3" {
		t.Errorf("GenerationID = %q", g)
	}
	if g := ToolID("s", "tc_1"); g != "tool:s:tc_1" {
		t.Errorf("ToolID = %q", g)
	}
	if g := ScoreID("tr", "tool_call_count"); g != "score:tr:tool_call_count" {
		t.Errorf("ScoreID = %q", g)
	}
}

func TestEventIDDeterministic(t *testing.T) {
	a := EventID("trace-create", "rid-1")
	b := EventID("trace-create", "rid-1")
	if a != b {
		t.Errorf("EventID non-deterministic: %q vs %q", a, b)
	}
	if len(a) != 36 {
		t.Errorf("EventID not UUID-shaped: %q", a)
	}
	// Different kind or resource → different id.
	c := EventID("generation-create", "rid-1")
	if a == c {
		t.Errorf("EventID not sensitive to kind")
	}
}
