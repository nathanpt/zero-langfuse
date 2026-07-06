// Command zero-langfuse turns Zero's persisted session log into Langfuse traces
// (DESIGN §1). Phase 1 adds `trace` (post one session) and `sync` (backfill
// many), both building Langfuse ingestion batches from the session log via
// REST (DESIGN §5.2) with self-computed cache-aware cost (§7) and privacy
// presets + redaction (§8). `dump` from Phase 0 remains.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nathanpt/zero-langfuse/internal/capture"
	"github.com/nathanpt/zero-langfuse/internal/config"
	"github.com/nathanpt/zero-langfuse/internal/langfuse"
	"github.com/nathanpt/zero-langfuse/internal/pricing"
	"github.com/nathanpt/zero-langfuse/internal/session"
	"github.com/nathanpt/zero-langfuse/internal/syncstate"
	"github.com/nathanpt/zero-langfuse/internal/trace"
)

const usage = `zero-langfuse — Langfuse observability for Zero (session-log reader)

Phase 1: ` + "`dump`" + ` (inspect), ` + "`trace`" + ` (post one session), ` + "`sync`" + `
(backfill many), ` + "`setup`" + ` (enter credentials), ` + "`test`" + ` (connectivity probe),
and ` + "`status`" + ` (config summary). watch mode is Phase 2 (DESIGN §15), deferred.

Usage:
  zero-langfuse dump <sessionId|dir|events.jsonl> [flags]
  zero-langfuse dump --latest [flags]
  zero-langfuse trace <sessionId|--latest> [--dry-run] [--privacy <preset>]
  zero-langfuse sync [--since <YYYY-MM-DD>] [--limit N] [--dry-run]
  zero-langfuse setup [--host <url> --public-key <pk> --secret-key <sk>]
  zero-langfuse test
  zero-langfuse status

Subcommands:
  dump    Pretty-print a session's metadata.json and every events.jsonl event,
          with a per-type count summary (Phase 0 validation tool).
  trace   Build Langfuse traces for one session and POST them (idempotent). One
          trace per user turn, generation per provider_usage with cache-aware
          cost, tool spans, trace scores; redacted per the active privacy preset.
  sync    Backfill all sessions under the sessions dir (idempotent upserts;
          bounded by --since/--limit).
  setup   Enter Langfuse host + keys interactively and save them to the config
          file (0600). Flags --host/--public-key/--secret-key or env vars skip
          the prompts; --secret-key avoids typing the key visibly.
  test    POST one probe trace to confirm the host + credentials work.
  status  Print the resolved config (host, masked keys, privacy, sessions count)
          without uploading anything.

Common flags:
  --latest           Act on the most recently active session (trace/dump).
  --sessions <dir>   Override the sessions directory
                     (default: $XDG_DATA_HOME/zero/sessions).
  --privacy <preset> Override the privacy preset for this run
                     (metadata-only|prompts-only|conversations|full-debug).
  --dry-run          Print the ingestion batch as JSON without posting (no creds
                     needed).
  -h, --help         Show this help.

Examples:
  zero-langfuse dump --latest
  zero-langfuse trace --latest --dry-run
  zero-langfuse trace --latest --privacy metadata-only
  zero-langfuse sync --since 2026-07-01 --limit 50
`

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "dump":
		cmdDump(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "trace":
		cmdTrace(os.Args[2:])
	case "setup":
		cmdSetup(os.Args[2:])
	case "test":
		cmdTest(os.Args[2:])
	case "sync":
		cmdSync(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "version", "-v", "--version":
		fmt.Printf("zero-langfuse %s\n", version)
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

	dir := resolveSessionsDir(*sessionsDir)

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

func cmdTrace(args []string) {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	latest := fs.Bool("latest", false, "trace the most recently active session")
	sessionsDir := fs.String("sessions", "", "sessions directory override")
	privacy := fs.String("privacy", "", "override privacy preset (metadata-only|prompts-only|conversations|full-debug)")
	dryRun := fs.Bool("dry-run", false, "print the batch as JSON without posting")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)

	cfg, err := config.Load(envMap())
	if err != nil {
		fail(err)
	}
	dir := resolveSessionsDirWithCfg(*sessionsDir, cfg)

	var target string
	if *latest {
		id, err := session.FindLatest(dir)
		if err != nil {
			fail(err)
		}
		target = id
	} else if fs.NArg() == 0 {
		fail(fmt.Errorf("trace requires a session id/dir/file, or --latest"))
	} else {
		target = fs.Arg(0)
	}

	sess, err := session.Load(target, dir)
	if err != nil {
		fail(err)
	}

	pol := resolvePolicy(cfg, *privacy)
	turns, evs, cost := buildSessionEvents(sess, pol, cfg.Pricing)

	if *dryRun {
		emitDryRun(sess.ID, turns, evs, cost)
		return
	}

	if err := cfg.Validate(); err != nil {
		fail(err)
	}
	client := langfuse.NewClient(cfg.Host, cfg.PublicKey, cfg.SecretKey)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := client.Ingest(ctx, evs); err != nil {
		fail(err)
	}
	obs := observationCount(evs)
	fmt.Printf("traced session %s: %d turns, %d observations, $%.6f\n", sess.ID, turns, obs, cost)
}

func cmdSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	sessionsDir := fs.String("sessions", "", "sessions directory override")
	since := fs.String("since", "", "only sessions created on/after this date (YYYY-MM-DD)")
	limit := fs.Int("limit", 0, "cap the number of sessions posted (0 = all)")
	dryRun := fs.Bool("dry-run", false, "print batches as JSON without posting")
	force := fs.Bool("force", false, "re-post sessions even if unchanged since last sync")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)

	cfg, err := config.Load(envMap())
	if err != nil {
		fail(err)
	}
	dir := resolveSessionsDirWithCfg(*sessionsDir, cfg)
	if dir == "" {
		fail(fmt.Errorf("no sessions directory resolved (pass --sessions or set XDG_DATA_HOME)"))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		fail(fmt.Errorf("read sessions dir %s: %w", dir, err))
	}

	// Collect candidates with their createdAt (newest first), peeking only
	// metadata.json so --since can skip the full load for old sessions.
	type candidate struct {
		name, createdAt string
	}
	var cands []candidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		created := peekCreatedAt(filepath.Join(dir, e.Name()))
		if !sinceOK(created, *since) {
			continue
		}
		cands = append(cands, candidate{name: e.Name(), createdAt: created})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].createdAt > cands[j].createdAt })

	var client *langfuse.Client
	if !*dryRun {
		if err := cfg.Validate(); err != nil {
			fail(err)
		}
		client = langfuse.NewClient(cfg.Host, cfg.PublicKey, cfg.SecretKey)
	}
	pol := resolvePolicy(cfg, "")

	// Cursor: skip sessions whose events.jsonl is unchanged since the last
	// successful sync (the log is append-only, so mtime advancing = new data).
	statePath, err := syncstate.Path()
	if err != nil {
		fail(err)
	}
	state, err := syncstate.Load(statePath)
	if err != nil {
		fail(err)
	}

	posted := 0
	totalObs := 0
	totalCost := 0.0
	for _, c := range cands {
		if *limit > 0 && posted >= *limit {
			break
		}
		// Cursor: skip unchanged sessions (unless --force). Applies to dry-run
		// too so it previews exactly what a real run would post.
		mtime := eventsMtime(filepath.Join(dir, c.name))
		if !*force && !state.ShouldPost(c.name, mtime) {
			fmt.Fprintf(os.Stderr, "skip %s: unchanged since last sync (--force to re-post)\n", c.name)
			continue
		}
		sess, err := session.Load(c.name, dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skip %s: %v\n", c.name, err)
			continue
		}
		turns, evs, cost := buildSessionEvents(sess, pol, cfg.Pricing)

		if *dryRun {
			emitDryRun(sess.ID, turns, evs, cost)
			posted++
			totalObs += observationCount(evs)
			totalCost += cost
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		if err := client.Ingest(ctx, evs); err != nil {
			cancel()
			fmt.Fprintf(os.Stderr, "warning: %s failed: %v\n", c.name, err)
			continue
		}
		cancel()
		state.Mark(sess.ID, mtime)
		posted++
		totalObs += observationCount(evs)
		totalCost += cost
		fmt.Fprintf(os.Stderr, "synced %s: %d turns, %d obs, $%.6f\n", sess.ID, turns, observationCount(evs), cost)
	}
	if !*dryRun {
		if err := syncstate.Save(statePath, state); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save sync state: %v\n", err)
		}
	}
	fmt.Printf("synced %d session(s), %d observations, $%.6f\n", posted, totalObs, totalCost)
}

// buildSessionEvents segments + builds all Langfuse events for one session,
// returning the turn count, events, and total computed cost.
func buildSessionEvents(sess *session.Session, pol capture.Policy, priceOverrides map[string]pricing.TokenPrice) (turns int, evs []langfuse.Event, cost float64) {
	for _, tn := range trace.Segment(sess) {
		tevs := trace.Build(tn, sess.Metadata, pol, priceOverrides)
		evs = append(evs, tevs...)
		turns++
		cost += batchCost(tevs)
	}
	return turns, evs, cost
}

// batchCost sums generation costDetails.total over a batch.
func batchCost(evs []langfuse.Event) float64 {
	var total float64
	for _, e := range evs {
		if e.Type != "generation-create" {
			continue
		}
		if ob, ok := e.Body.(langfuse.ObsBody); ok && ob.CostDetails != nil {
			total += ob.CostDetails["total"]
		}
	}
	return total
}

func observationCount(evs []langfuse.Event) int {
	n := 0
	for _, e := range evs {
		if e.Type == "generation-create" || e.Type == "span-create" {
			n++
		}
	}
	return n
}

// emitDryRun prints the ingestion payload as indented JSON to stdout and a
// per-batch summary to stderr (stdout stays parseable JSON).
func emitDryRun(sessionID string, turns int, evs []langfuse.Event, cost float64) {
	payload := struct {
		Batch    []langfuse.Event  `json:"batch"`
		Metadata map[string]string `json:"metadata"`
	}{
		Batch:    evs,
		Metadata: map[string]string{"source": "zero-langfuse"},
	}
	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(out))
	fmt.Fprintf(os.Stderr, "session %s: %d turn(s), %d observation(s), $%.6f\n", sessionID, turns, observationCount(evs), cost)
}

// resolvePolicy builds the capture policy: --privacy (if set) wins, else env
// LANGFUSE_PRIVACY_PRESET, else the config file preset. Fine-grained
// LANGFUSE_CAPTURE_* env flags always apply on top.
func resolvePolicy(cfg *config.Config, privacyFlag string) capture.Policy {
	env := envMap()
	if privacyFlag != "" {
		env["LANGFUSE_PRIVACY_PRESET"] = privacyFlag
	}
	return capture.FromEnv(env, capture.NormalizePreset(cfg.Privacy))
}

func resolveSessionsDir(flagDir string) string {
	if flagDir != "" {
		return flagDir
	}
	dir, err := session.DefaultSessionsDir()
	if err != nil {
		fail(err)
	}
	return dir
}

func resolveSessionsDirWithCfg(flagDir string, cfg *config.Config) string {
	if flagDir != "" {
		return flagDir
	}
	if cfg.SessionsDir != "" {
		return cfg.SessionsDir
	}
	dir, err := session.DefaultSessionsDir()
	if err != nil {
		return ""
	}
	return dir
}

// peekCreatedAt reads only metadata.json (not the full session) for its
// createdAt field, so --since can filter without loading events.jsonl.
func peekCreatedAt(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return ""
	}
	var m struct {
		CreatedAt string `json:"createdAt"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.CreatedAt
}

// eventsMtime returns the events.jsonl mtime (Unix nanoseconds), the sync
// cursor's growth signal. Falls back to metadata.json, then 0.
func eventsMtime(dir string) int64 {
	fi, err := os.Stat(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		fi, err = os.Stat(filepath.Join(dir, "metadata.json"))
		if err != nil {
			return 0
		}
	}
	return fi.ModTime().UnixNano()
}

func sinceOK(createdAt, since string) bool {
	if since == "" {
		return true
	}
	ct, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return true // unparseable → don't filter out
	}
	day, err := time.Parse("2006-01-02", since)
	if err != nil {
		return true // invalid --since → don't filter
	}
	return !ct.Before(day)
}

func envMap() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}

func cmdSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	var fHost, fPub, fSec, fPrivacy string
	force := fs.Bool("force", false, "overwrite an existing config without confirming")
	fs.StringVar(&fHost, "host", "", "Langfuse host URL")
	fs.StringVar(&fPub, "public-key", "", "Langfuse public key (pk-lf-\u2026)")
	fs.StringVar(&fSec, "secret-key", "", "Langfuse secret key (sk-lf-\u2026); use this flag or LANGFUSE_SECRET_KEY to avoid typing it visibly")
	fs.StringVar(&fPrivacy, "privacy", "", "privacy preset (metadata-only|prompts-only|conversations|full-debug)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)

	env := envMap()
	existing, err := config.LoadFileOnly()
	if err != nil {
		fail(err)
	}
	if existing != nil && existing.PublicKey != "" && !*force {
		fmt.Fprintf(os.Stderr, "a config already exists at %s; re-run with --force to overwrite, or edit it directly.\n", mustConfigPath())
		return
	}

	in := bufio.NewReader(os.Stdin)
	hostDef := defaultConfigHost(existing)
	privacyDef := defaultConfigPrivacy(existing)

	host := firstNonEmpty(fHost, env["LANGFUSE_BASE_URL"], env["LANGFUSE_HOST"])
	if host == "" {
		host = prompt(in, "Langfuse host", hostDef)
	}
	pub := firstNonEmpty(fPub, env["LANGFUSE_PUBLIC_KEY"])
	if pub == "" {
		pub = prompt(in, "Public key (pk-lf-\u2026)", existingPubDefault(existing))
	}
	sec := firstNonEmpty(fSec, env["LANGFUSE_SECRET_KEY"])
	if sec == "" {
		sec = prompt(in, "Secret key (sk-lf-\u2026, echoed \u2014 use --secret-key to hide)", "")
	}
	privacy := firstNonEmpty(fPrivacy, env["LANGFUSE_PRIVACY_PRESET"])
	if privacy == "" {
		privacy = prompt(in, "Privacy preset [metadata-only|prompts-only|conversations|full-debug]", privacyDef)
	}

	c := config.MergeSetup(existing, host, pub, sec, privacy)
	if err := c.Validate(); err != nil {
		fail(err)
	}
	if err := config.Save(c); err != nil {
		fail(err)
	}
	path := mustConfigPath()
	fmt.Printf("saved config \u2192 %s\n  host:      %s\n  publicKey: %s\n  secretKey: %s\n  privacy:   %s\n",
		path, c.Host, config.Mask(c.PublicKey), strings.Repeat("*", 8), c.Privacy)
	fmt.Fprintln(os.Stderr, "run `zero-langfuse test` to verify connectivity.")
}

func cmdTest(args []string) {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)

	cfg, err := config.Load(envMap())
	if err != nil {
		fail(err)
	}
	if err := cfg.Validate(); err != nil {
		fail(err)
	}
	client := langfuse.NewClient(cfg.Host, cfg.PublicKey, cfg.SecretKey)
	// A deterministic test trace so re-running `test` overwrites the same row
	// rather than accumulating clutter (idempotent via the fixed id).
	traceID := langfuse.TraceID("zero-langfuse-test", 0)
	ts := time.Now().UTC().Format(time.RFC3339)
	ev := langfuse.Event{
		Type:      "trace-create",
		ID:        langfuse.EventID("trace-create", traceID),
		Timestamp: ts,
		Body: langfuse.TraceBody{
			ID:        traceID,
			Timestamp: ts,
			Name:      "zero-langfuse connectivity test",
			SessionID: "zero-langfuse-test",
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fmt.Fprintf(os.Stderr, "probing %s \u2026\n", cfg.Host)
	if err := client.Ingest(ctx, []langfuse.Event{ev}); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK: connected to %s \u2014 auth accepted, ingestion round-trip succeeded.\n", cfg.Host)
	fmt.Fprintln(os.Stderr, "(one probe trace \"zero-langfuse connectivity test\" was created; re-running overwrites it.)")
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)

	cfg, err := config.Load(envMap())
	if err != nil {
		fail(err)
	}
	path := mustConfigPath()

	fmt.Printf("config:   %s\n", path)
	fmt.Printf("host:     %s\n", cfg.Host)
	fmt.Printf("pubKey:   %s\n", maskedOrUnset(cfg.PublicKey))
	fmt.Printf("secKey:   %s\n", setOrUnset(cfg.SecretKey))
	fmt.Printf("privacy:  %s\n", cfg.Privacy)
	fmt.Printf("flushAt:  %d\n", cfg.FlushAt)
	if n := len(cfg.Pricing); n > 0 {
		fmt.Printf("pricing:  %d model override(s)\n", n)
	} else {
		fmt.Printf("pricing:  (bundled table only)\n")
	}

	if cfg.SessionsDir != "" {
		count := sessionsSummary(cfg.SessionsDir)
		if count >= 0 {
			fmt.Printf("sessions: %d under %s\n", count, cfg.SessionsDir)
		} else {
			fmt.Printf("sessions: dir not found (%s)\n", cfg.SessionsDir)
		}
	}

	if cfg.PublicKey == "" || cfg.SecretKey == "" {
		fmt.Fprintln(os.Stderr, "\ncredentials not set — run `zero-langfuse setup`.")
	} else {
		fmt.Fprintln(os.Stderr, "\nrun `zero-langfuse test` to verify connectivity.")
	}
}

func maskedOrUnset(k string) string {
	if k == "" {
		return "(unset)"
	}
	return config.Mask(k)
}

func setOrUnset(k string) string {
	if k == "" {
		return "(unset)"
	}
	return "set"
}

// sessionsSummary returns the count of session subdirectories, or -1 if the
// dir cannot be read.
func sessionsSummary(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}

func prompt(in *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	line, err := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line != "" {
		return line
	}
	if err != nil {
		return def // EOF / no input → fall back to default
	}
	return def
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func mustConfigPath() string {
	p, err := config.ConfigPath()
	if err != nil {
		return "(unknown)"
	}
	return p
}

func defaultConfigHost(existing *config.Config) string {
	if existing != nil && existing.Host != "" {
		return existing.Host
	}
	return "https://cloud.langfuse.com"
}

func defaultConfigPrivacy(existing *config.Config) string {
	if existing != nil && existing.Privacy != "" {
		return existing.Privacy
	}
	return "full-debug"
}

func existingPubDefault(existing *config.Config) string {
	if existing != nil {
		return existing.PublicKey
	}
	return ""
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
