package session

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Dump writes a human-readable rendering of the session to w:
//   - metadata.json, re-pretty-printed (if present),
//   - every event as a header (line/type/sequence/timestamp) plus its payload
//     re-pretty-printed,
//   - a per-type event-count summary.
//
// When filterType is non-empty, only events of that type are printed (the
// summary still reflects all events). When summaryOnly is true, only the counts
// table is written.
//
// This is the Phase 0 validation tool: its job is to reveal the real event
// shapes (DESIGN §13 Q1–Q3), so payloads are dumped verbatim, not interpreted.
func (s *Session) Dump(w io.Writer, filterType string, summaryOnly bool) error {
	var b strings.Builder

	if !summaryOnly {
		s.dumpMetadata(&b)
	}

	// Per-type counts over ALL events (independent of filter).
	counts := make(map[string]int)
	for _, ev := range s.Events {
		name := ev.Type
		if name == "" {
			name = "(no type)"
		}
		counts[name]++
	}

	if !summaryOnly {
		shown := 0
		for _, ev := range s.Events {
			if filterType != "" && ev.Type != filterType {
				continue
			}
			shown++
			dumpEvent(&b, ev)
		}
		if shown == 0 && len(s.Events) > 0 {
			fmt.Fprintf(&b, "(no events of type %q)\n\n", filterType)
		}
	}

	writeSummary(&b, counts, len(s.Events))
	_, err := io.WriteString(w, b.String())
	return err
}

func (s *Session) dumpMetadata(b *strings.Builder) {
	fmt.Fprintf(b, "session: %s\n", s.ID)
	fmt.Fprintf(b, "dir:     %s\n", s.Dir)
	if len(s.MetadataRaw) == 0 {
		fmt.Fprintln(b, "metadata: (absent)")
	} else {
		fmt.Fprintln(b, "metadata.json:")
		if pretty, err := prettyJSON(s.MetadataRaw); err == nil {
			writeIndented(b, pretty)
		} else {
			// Fall back to the raw bytes if re-indenting fails.
			b.Write(s.MetadataRaw)
			b.WriteByte('\n')
		}
	}
	fmt.Fprintln(b)
	fmt.Fprintf(b, "events: %d parsed, %d torn/skipped\n", len(s.Events), len(s.TornLines))
	if len(s.TornLines) > 0 {
		fmt.Fprintf(b, "torn lines: %v\n", intsToStr(s.TornLines))
	}
	fmt.Fprintln(b)
}

func dumpEvent(b *strings.Builder, ev *Event) {
	fmt.Fprintf(b, "──[#%d] %s", ev.Line, ev.Type)
	if ev.Sequence != 0 {
		fmt.Fprintf(b, " seq=%d", ev.Sequence)
	}
	if ev.CreatedAt != "" {
		fmt.Fprintf(b, " ts=%s", ev.CreatedAt)
	}
	if s := ev.summaryPeek(); s != "" {
		fmt.Fprintf(b, " · %s", s)
	}
	fmt.Fprintln(b, " ──")
	switch {
	case len(ev.Payload) > 0:
		if pretty, err := prettyJSON(ev.Payload); err == nil {
			writeIndented(b, pretty)
		} else {
			b.Write(ev.Payload)
			b.WriteByte('\n')
		}
	default:
		fmt.Fprintln(b, "(no payload)")
	}
	fmt.Fprintln(b)
}

func writeSummary(b *strings.Builder, counts map[string]int, total int) {
	fmt.Fprintf(b, "summary: %d event(s)\n", total)
	if total == 0 {
		return
	}
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool {
		if counts[types[i]] != counts[types[j]] {
			return counts[types[i]] > counts[types[j]]
		}
		return types[i] < types[j]
	})
	for _, t := range types {
		fmt.Fprintf(b, "  %-22s %d\n", t, counts[t])
	}
}

// prettyJSON re-indents JSON with HTML escaping disabled for readability.
func prettyJSON(raw json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return out, nil
}

func writeIndented(b *strings.Builder, raw json.RawMessage) {
	b.Write(raw)
	b.WriteByte('\n')
}

func intsToStr(xs []int) string {
	var sb strings.Builder
	for i, x := range xs {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%d", x)
	}
	return sb.String()
}
