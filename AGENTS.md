# AGENTS.md

Operating manual for AI coding agents (and humans) working on **zero-langfuse**. Read this before
making changes. The authoritative design is [`DESIGN.md`](./DESIGN.md) — this file is the short
operating summary; when they disagree, DESIGN.md wins.

## What this is

Langfuse observability for **[Zero](https://github.com/Gitlawb/zero)** (Gitlawb's terminal coding
agent). A small **Go binary** that reads Zero's own persisted session log and turns each user turn
into a Langfuse trace — one ingestion path that serves Zero's **TUI, `zero exec`, and specialist
sub-agents** uniformly. Sibling to [`omp-langfuse`](https://github.com/nathanpt/omp-langfuse); we
reuse its principles (self-computed cost, REST ingestion, privacy presets, redaction) but the host
forces a different architecture.

> **Status: pre-implementation.** DESIGN.md is complete and source-verified; no code exists yet.
> First task is Phase 0 (below).

## The core decision (do not re-litigate)

Zero has no in-process extension host. Instead of wrapping `zero exec` and parsing stream-json
(v1's plan, now obsolete), **read the session log** — a complete, fsync'd, append-only record that
Zero writes for *every* run surface, with a token breakdown richer than stream-json exposes:

```
$XDG_DATA_HOME/zero/sessions/<sessionId>/
├── metadata.json     # sessionId, modelId, provider, cwd, title, parentSessionId, rootSessionId…
└── events.jsonl      # message · tool_call · tool_result · permission · provider_usage · error · specialist_* …
```

The `provider_usage` event carries `promptTokens, completionTokens, totalTokens, cachedInputTokens,
cacheWriteTokens, reasoningTokens, model` — enough for cache-aware cost. Verified written by exec
(`internal/cli/exec.go:583`), TUI (`internal/tui/model.go:4496`), and specialists
(`internal/specialist/accounting.go:105`).

## Critical constraints (do not violate)

1. **REST ingestion only — no OTel, no `@langfuse/*` SDK.** omp-langfuse's worst bug was Bun failing
   to resolve the OTel dependency graph, fixed only by esbuild-bundling. An out-of-process reader has
   no reason to use OTel. Direct `POST /api/public/ingestion` (Basic auth), batched + retried.
2. **The session log is the source of truth.** Do not parse stream-json as the primary path and do
   not rely on hooks — hooks carry no usage (`internal/hooks` has zero token refs), and the
   stream-json wire emits only prompt/completion/total (no cost, no cache breakdown). stream-json is
   an *optional* exec-only live accelerator (DESIGN §10), nothing more.
3. **Always self-compute cost** from tokens × resolved price. Zero never exposes cost on any external
   surface. Never invent a "host cost" field.
4. **Redact before upload — it is entirely our job.** The session log is Zero's un-redacted private
   transcript. Apply the omp-langfuse redaction patterns (API keys, bearer tokens, cookies, private
   keys, GitHub/npm/AWS tokens, local absolute paths) before anything leaves the machine.
5. **Idempotency.** Trace/observation ids must be derived from `(sessionId, sequence)` so `sync` and
   crash-restart never duplicate traces in Langfuse.
6. **Don't modify Zero.** The session log already has everything we need; no upstream fork/PR is
   required.

## Dev workflow

Go. Single binary. (No `go.mod` exists yet — Phase 0 creates it.)

```bash
go build ./...          # build
go test ./...           # unit tests
go run . watch          # tail active sessions (once wired)
go run . dump <sid>     # Phase 0 tool: pretty-print a session's events.jsonl + metadata.json
```

All of `go build` / `go test` must stay green before a change is merged.

Credentials/config live outside any repo: `$XDG_CONFIG_HOME/zero-langfuse/config.json` (0600 in a
0700 dir), or env vars `LANGFUSE_PUBLIC_KEY` / `LANGFUSE_SECRET_KEY` / `LANGFUSE_BASE_URL`. Do **not**
commit credentials.

## Verification (for changes touching the trace/cost path)

Unit tests are not enough for trace-path work (omp-langfuse lesson). Before merging, validate against
a **real Langfuse**:

1. `go run . watch`, then run an interactive `zero` turn that uses a tool + one that fails.
2. In Langfuse confirm: one trace per turn, generation `usageDetails` with cache fields + populated
   `costDetails`, tool observation `isError=true` on the failure, trace-level scores, correct
   session grouping across turns.
3. Repeat for `zero exec … ; go run . trace --latest`. Confirm `sync` run twice produces no
   duplicates.

See DESIGN §14.

## Phase 0 (the first thing to build)

A `dump` command that reads a session's `events.jsonl` + `metadata.json` and pretty-prints every
event type with its payload. Its real purpose is to **settle DESIGN §13 Q1–Q3 against live TUI +
exec captures** before any Langfuse code is written:

- **Q1 — trace segmentation:** confirm every run (esp. the TUI) appends a user `message` marker
  before its assistant work, so "one trace per turn" segmentation holds. Verified for exec only.
- **Q2 — `EffectiveInputTokens()` cache semantics:** confirm whether persisted `promptTokens`
  includes or excludes cached input (decides whether the cost formula double-counts).
- **Q3 — assistant-message completeness:** confirm the assistant `message` carries the full answer
  text (one read, not delta reassembly).

These three gate the design. Do not start Phase 1 until they're answered.

## Carry-over source

[`omp-langfuse`](https://github.com/nathanpt/omp-langfuse) (esp. `.docs/DESIGN.md` §8.1 cost,
`src/pricing.ts`, `src/redaction.ts`, `src/capture-policy.ts`) is the source for the pricing table,
price-resolution precedence, redaction patterns, and privacy presets. Port the *logic* to Go; do not
introduce a Node/TS dependency.

## Conventions

- Branch per story (`phase0/session-dumper`, `trace/segmentation`), merge to `main` with `--no-ff`.
- Imperative commit messages; the CHANGELOG (once it exists) carries intent, not commit subjects.
- Curated, intentional releases (DESIGN §11) — no auto-versioning tooling.
