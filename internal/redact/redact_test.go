package redact

import (
	"strings"
	"testing"
)

func TestRedactBearer(t *testing.T) {
	in := "auth: Bearer abcdefghijklmnop1234567890"
	out := RedactString(in, DefaultRedactOptions)
	if strings.Contains(out, "abcdefghijklmnop1234567890") {
		t.Errorf("bearer leaked: %q", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Errorf("bearer not masked: %q", out)
	}
}

func TestRedactPrivateKey(t *testing.T) {
	in := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----"
	out := RedactString(in, DefaultRedactOptions)
	if strings.Contains(out, "MIIEowIBAAKCAQEA") {
		t.Errorf("private key leaked: %q", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Errorf("private key not masked: %q", out)
	}
}

func TestRedactKnownTokens(t *testing.T) {
	cases := []string{
		"key: sk-lf-abcdef123456",
		"key: sk-ant-abcdef123456",
		"key: pk-lf-somekeyvalue123",
		"key: ghp_" + strings.Repeat("a", 24),
		"key: npm_" + strings.Repeat("b", 22),
		"key: AKIA" + "IOSFODNN7EXAMPLE"[:16],
	}
	for _, in := range cases {
		out := RedactString(in, DefaultRedactOptions)
		// After masking, the raw secret substring must be gone.
		secret := strings.TrimSpace(strings.TrimPrefix(in, "key: "))
		if strings.Contains(out, secret) {
			t.Errorf("token leaked (%q): %q", secret, out)
		}
	}
}

func TestRedactSecretAssignment(t *testing.T) {
	in := "API_KEY=sk-live-abcdef123456"
	out := RedactString(in, DefaultRedactOptions)
	// Key name kept, value masked.
	if !strings.HasPrefix(out, "API_KEY=") {
		t.Errorf("key name not preserved: %q", out)
	}
	if strings.Contains(out, "sk-live-abcdef123456") {
		t.Errorf("assignment value leaked: %q", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Errorf("value not masked: %q", out)
	}
}

func TestRedactAbsolutePathHashed(t *testing.T) {
	in := "reading /home/bob/secret.txt now"
	out := RedactString(in, DefaultRedactOptions)
	if strings.Contains(out, "/home/bob/secret.txt") {
		t.Errorf("absolute path leaked: %q", out)
	}
	if !strings.Contains(out, "[PATH_HASH:") {
		t.Errorf("path not hashed: %q", out)
	}
	// Same input → same hash (stable).
	h := HashPath("/home/bob/secret.txt")
	if !strings.Contains(out, h) {
		t.Errorf("hash mismatch: want %q in %q", h, out)
	}
}

func TestRedactEnvSuffixPreserved(t *testing.T) {
	in := "config at /home/alice/.env.production"
	out := RedactString(in, DefaultRedactOptions)
	if strings.Contains(out, "/home/alice") {
		t.Errorf("path leaked: %q", out)
	}
	// The suffix is preserved verbatim after the hash.
	if !strings.Contains(out, ".env.production") {
		t.Errorf("env suffix not preserved: %q", out)
	}
}

func TestRedactTruncate(t *testing.T) {
	long := strings.Repeat("a", 20000)
	out := RedactString(long, DefaultRedactOptions)
	if !strings.Contains(out, "[truncated]") {
		t.Errorf("long string not truncated: len=%d", len(out))
	}
}

func TestRedactSensitiveFieldValue(t *testing.T) {
	in := map[string]any{
		"name":          "alice",
		"Authorization": "Bearer supersecretvalue1234",
		"apiKey":        "sk-lf-xyz123abc456",
		"password":      "hunter2",
	}
	out := RedactValue(in, DefaultRedactOptions).(map[string]any)
	if out["name"] != "alice" {
		t.Errorf("benign field altered: %v", out["name"])
	}
	// Sensitive keys → value replaced wholesale.
	if out["Authorization"] != Redacted {
		t.Errorf("Authorization value not masked: %v", out["Authorization"])
	}
	if out["apiKey"] != Redacted {
		t.Errorf("apiKey value not masked: %v", out["apiKey"])
	}
	if out["password"] != Redacted {
		t.Errorf("password value not masked: %v", out["password"])
	}
}

func TestRedactNestedSecretInString(t *testing.T) {
	in := map[string]any{"note": "token is Bearer abcdefghijklmnop1234"}
	out := RedactValue(in, DefaultRedactOptions).(map[string]any)
	if strings.Contains(out["note"].(string), "abcdefghijklmnop1234") {
		t.Errorf("secret leaked in string value: %v", out["note"])
	}
}

func TestRedactDepthLimit(t *testing.T) {
	o := RedactOptions{MaxDepth: 2, MaxArrayItems: 50, MaxObjectKeys: 80, MaxStringLength: 12000}
	// depth 1: {a:{b:{c:1}}} → at depth 2, the innermost object {c:1} is
	// reached at depth 1, its children visited at depth 0 → {c:1} survives, but
	// a deeper level would sentinel.
	in := map[string]any{"a": map[string]any{"b": map[string]any{"c": 1}}}
	out := RedactValue(in, o)
	got := out.(map[string]any)
	// Level 1 (depth2): {a:...}. Level 2 (depth1): {b:...}. Level 3 (depth0): {c:1} → sentinel.
	a := got["a"].(map[string]any)
	if _, ok := a["b"].(string); !ok || !strings.HasPrefix(a["b"].(string), "[max depth") {
		t.Errorf("depth sentinel missing; got %v", a["b"])
	}
}

func TestRedactArrayLimit(t *testing.T) {
	o := RedactOptions{MaxDepth: 6, MaxArrayItems: 3, MaxObjectKeys: 80, MaxStringLength: 12000}
	in := []any{1, 2, 3, 4, 5}
	out := RedactValue(in, o).([]any)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4 (3 items + sentinel)", len(out))
	}
	if out[0] != 1 || out[1] != 2 || out[2] != 3 {
		t.Errorf("items = %v, want [1 2 3 sentinel]", out)
	}
	if s, ok := out[3].(string); !ok || !strings.Contains(s, "2 truncated items") {
		t.Errorf("sentinel = %v, want count", out[3])
	}
}

func TestRedactObjectKeyLimit(t *testing.T) {
	o := RedactOptions{MaxDepth: 6, MaxArrayItems: 50, MaxObjectKeys: 2, MaxStringLength: 12000}
	in := map[string]any{"a": 1, "b": 2, "c": 3, "d": 4}
	out := RedactValue(in, o).(map[string]any)
	if cnt := len(out); cnt != 3 { // 2 keys + __truncatedKeys
		t.Fatalf("len = %d, want 3 (2 keys + __truncatedKeys): %v", cnt, out)
	}
	if n, ok := out["__truncatedKeys"]; !ok || n != 2 {
		t.Errorf("__truncatedKeys = %v, want 2", out["__truncatedKeys"])
	}
}

// A self-referential map must be cut by cycle detection, not infinite-loop.
func TestRedactCyclic(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	out := RedactValue(m, DefaultRedactOptions).(map[string]any)
	if out["self"] != "[circular]" {
		t.Errorf("cycle not detected: %v", out["self"])
	}
}
