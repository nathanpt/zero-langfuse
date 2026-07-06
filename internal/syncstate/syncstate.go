// Package syncstate records which sessions sync has already posted, so re-runs
// skip unchanged sessions instead of re-posting them (idempotent upserts are
// still correct, but wasteful). It keys on the events.jsonl mtime: the session
// log is append-only, so an advancing mtime is a reliable growth signal.
//
// Phase 1 sync re-posted every matching session each run; this is the
// "last-synced cursor" optimization noted as deferred there.
package syncstate

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// State maps sessionId → events.jsonl mtime (Unix nanoseconds) at last post.
type State map[string]int64

// Path returns the sync state file under XDG_STATE_HOME (default
// ~/.local/state/zero-langfuse/sync.json).
func Path() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "zero-langfuse", "sync.json"), nil
}

// Load reads the state file. A missing file yields an empty State (not an error)
// so a first run is clean.
func Load(path string) (State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if s == nil {
		s = State{}
	}
	return s, nil
}

// Save writes the state file (parent 0700, file 0600).
func Save(path string, s State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// ShouldPost reports whether the session should be (re)posted: true when not
// previously recorded or its mtime has changed since the last post.
func (s State) ShouldPost(sessionID string, mtime int64) bool {
	prev, ok := s[sessionID]
	return !ok || prev != mtime
}

// Mark records that a session was posted at the given mtime.
func (s State) Mark(sessionID string, mtime int64) {
	s[sessionID] = mtime
}
