// Command zero-langfuse turns Zero's persisted session log into Langfuse traces
// (DESIGN §1). This is the Phase 0 build: only `dump` exists, the validation
// tool that reads a session's events.jsonl + metadata.json and pretty-prints
// every event so the real shapes settle DESIGN §13 Q1–Q3.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nathanpt/zero-langfuse/internal/session"
)

const usage = `zero-langfuse — Langfuse observability for Zero (session-log reader)

Phase 0: only ` + "`dump`" + ` is implemented. Later phases add watch/trace/sync/setup
(DESIGN §15).

Usage:
  zero-langfuse dump <sessionId|dir|events.jsonl> [flags]
  zero-langfuse dump --latest [flags]

Subcommands:
  dump    Pretty-print a session's metadata.json and every events.jsonl event,
          with a per-type count summary. This is the Phase 0 validation tool:
          it reveals the real event shapes needed to settle trace segmentation,
          cache-token semantics, and assistant-message completeness (DESIGN §13).

Flags:
  --latest           Dump the most recently active session.
  --sessions <dir>   Override the sessions directory
                     (default: $XDG_DATA_HOME/zero/sessions).
  --type <type>      Only show events of this type (summary still counts all).
  --summary          Only print the per-type event counts.
  -h, --help         Show this help.

Examples:
  zero-langfuse dump --latest
  zero-langfuse dump abc-123 --type provider_usage
  zero-langfuse dump --summary
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "dump":
		cmdDump(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "version", "-v", "--version":
		// Printed by ldflags-injected version when present; static fallback now.
		fmt.Println("zero-langfuse dev (phase 0)")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func cmdDump(args []string) {
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	latest := fs.Bool("latest", false, "dump the most recently active session")
	sessionsDir := fs.String("sessions", "", "sessions directory override (default $XDG_DATA_HOME/zero/sessions)")
	filterType := fs.String("type", "", "only show events of this type")
	summaryOnly := fs.Bool("summary", false, "only print per-type event counts")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)

	dir := *sessionsDir
	if dir == "" {
		d, err := session.DefaultSessionsDir()
		if err != nil {
			fail(err)
		}
		dir = d
	}

	var target string
	if *latest {
		id, err := session.FindLatest(dir)
		if err != nil {
			fail(err)
		}
		target = id
	} else if fs.NArg() == 0 {
		fail(fmt.Errorf("dump requires a session id/dir/file, or --latest"))
	} else {
		target = fs.Arg(0)
	}

	sess, err := session.Load(target, dir)
	if err != nil {
		fail(err)
	}
	if err := sess.Dump(os.Stdout, *filterType, *summaryOnly); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
