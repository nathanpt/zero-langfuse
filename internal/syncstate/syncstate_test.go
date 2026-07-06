package syncstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathHonorsXDGStateHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "zero-langfuse", "sync.json")
	if p != want {
		t.Errorf("Path = %q, want %q", p, want)
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if len(s) != 0 {
		t.Errorf("expected empty State, got %v", s)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "sync.json")
	s := State{"sess-a": 111, "sess-b": 222}
	if err := Save(path, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// File is 0600.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perms = %o, want 0600", perm)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got["sess-a"] != 111 || got["sess-b"] != 222 || len(got) != 2 {
		t.Errorf("round-trip mismatch: %v", got)
	}
}

func TestShouldPost(t *testing.T) {
	s := State{"sess": 100}
	if !s.ShouldPost("unknown", 100) {
		t.Error("unknown session should post")
	}
	if !s.ShouldPost("sess", 200) {
		t.Error("changed mtime should post")
	}
	if s.ShouldPost("sess", 100) {
		t.Error("unchanged mtime should NOT post")
	}
}

func TestMark(t *testing.T) {
	s := State{}
	s.Mark("sess", 123)
	if s["sess"] != 123 {
		t.Errorf("Mark failed: %v", s)
	}
	if !s.ShouldPost("sess", 999) {
		t.Error("after Mark, a different mtime should still post")
	}
}
