package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	metadataFile = "metadata.json"
	eventsFile   = "events.jsonl"
)

// Session is a loaded session directory: its metadata.json (kept raw so `dump`
// can re-pretty-print the real schema) and the parsed events from events.jsonl.
type Session struct {
	// Dir is the resolved session directory on disk.
	Dir string
	// ID is the session id (directory base name, or metadata.sessionId if set).
	ID string
	// MetadataRaw is the raw bytes of metadata.json (nil if absent).
	MetadataRaw json.RawMessage
	// Metadata is the parsed metadata object (nil if absent).
	Metadata map[string]any
	// Events are the successfully parsed events, in file order.
	Events []*Event
	// TornLines are 1-based line numbers skipped as malformed/torn.
	TornLines []int
}

// DefaultSessionsDir returns Zero's session directory, honoring XDG_DATA_HOME
// (DESIGN §3): $XDG_DATA_HOME/zero/sessions, defaulting to
// ~/.local/share/zero/sessions.
func DefaultSessionsDir() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "zero", "sessions"), nil
}

// Load reads a session given a target, which may be:
//   - a session id resolved under sessionsDir (e.g. "abc123"),
//   - a path to the session directory,
//   - a path directly to its events.jsonl.
//
// metadata.json is optional (a session directory may exist briefly before it is
// flushed); events.jsonl must be present.
func Load(target, sessionsDir string) (*Session, error) {
	dir, err := resolveSessionDir(target, sessionsDir)
	if err != nil {
		return nil, err
	}
	return loadDir(dir)
}

// resolveSessionDir maps a target onto a session directory.
func resolveSessionDir(target, sessionsDir string) (string, error) {
	if target == "" {
		return "", errors.New("empty session target")
	}
	// Absolute or relative path: a directory, or a file (events.jsonl).
	if fi, err := os.Stat(target); err == nil {
		if !fi.IsDir() {
			// A file target is assumed to be events.jsonl; use its parent.
			return filepath.Dir(target), nil
		}
		return target, nil
	}
	// Otherwise treat target as a session id under sessionsDir.
	if sessionsDir == "" {
		return "", fmt.Errorf("session %q not found and no sessions directory given", target)
	}
	return filepath.Join(sessionsDir, target), nil
}

func loadDir(dir string) (*Session, error) {
	eventsPath := filepath.Join(dir, eventsFile)
	if _, err := os.Stat(eventsPath); err != nil {
		return nil, fmt.Errorf("no %s in %s: %w", eventsFile, dir, err)
	}

	s := &Session{Dir: dir, ID: filepath.Base(dir)}

	if raw, err := os.ReadFile(filepath.Join(dir, metadataFile)); err == nil {
		s.MetadataRaw = raw
		_ = json.Unmarshal(raw, &s.Metadata)
		if id, ok := s.Metadata["sessionId"].(string); ok && id != "" {
			s.ID = id
		}
	}

	events, torn, err := readEvents(eventsPath)
	if err != nil {
		return nil, err
	}
	s.Events = events
	s.TornLines = torn
	return s, nil
}

// FindLatest returns the session id whose events.jsonl (or, failing that,
// metadata.json) was most recently modified under sessionsDir. Used by
// `dump --latest`.
func FindLatest(sessionsDir string) (string, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("no sessions directory at %s (has zero run here? pass --sessions or set XDG_DATA_HOME)", sessionsDir)
		}
		return "", err
	}
	var best string
	var bestMT time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mt := modTime(filepath.Join(sessionsDir, e.Name(), eventsFile))
		if mt.IsZero() {
			mt = modTime(filepath.Join(sessionsDir, e.Name(), metadataFile))
		}
		if mt.IsZero() {
			continue
		}
		if mt.After(bestMT) {
			best, bestMT = e.Name(), mt
		}
	}
	if best == "" {
		return "", fmt.Errorf("no sessions under %s", sessionsDir)
	}
	return best, nil
}

func modTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
