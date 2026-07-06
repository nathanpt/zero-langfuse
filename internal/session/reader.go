package session

import (
	"bufio"
	"bytes"
	"os"
)

// maxLineBytes caps a single events.jsonl line. Tool results (file reads,
// command output) can be large; 8 MiB is generous while bounding memory. A line
// exceeding this is read in chunks and flagged torn rather than aborting.
const maxLineBytes = 8 << 20

// readEvents reads events.jsonl line by line, tolerating malformed/torn lines.
// DESIGN §6.3: a torn trailing line is normal (Zero fsyncs per append, so a
// partial write appears only at the very tail); the store ignores it, and so do
// we — skipping and reporting rather than aborting.
func readEvents(path string) (events []*Event, torn []int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)

	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		ev := parseEvent(line, raw)
		if ev == nil {
			torn = append(torn, line)
			continue
		}
		events = append(events, ev)
	}
	if err = sc.Err(); err != nil {
		return nil, torn, err
	}
	return events, torn, nil
}
