# Roadmap

Status and sequencing for zero-langfuse. **DESIGN.md is authoritative** for *what*
and *why*; this file tracks *what's done, what's next, and what's deferred*.
When they conflict, DESIGN.md wins. AGENTS.md is the operating summary.

Status labels: **Done** · **Deferred** (with re-trigger) · **Planned** · **Optional**.

---

## Current status

**Phase 0–1 + config slice + polish are complete, merged to `main`, and
live-verified against a self-hosted Langfuse.** The tool reads Zero's session
log and posts one Langfuse trace per user turn — generation per `provider_usage`
with self-computed cache-aware cost, tool spans with error detection, six
trace-level scores — idempotently, redacted per the active privacy preset.

`dump` (inspect), `trace` (post one session), `sync` (backfill, cursor-aware),
`setup`/`test`/`status` (credentials + connectivity) all ship.

**Watch mode (Phase 2) is deferred** — see below.

---

## By phase

### Phase 0 — Session-log reader & validation — **Done**
`dump` command; DESIGN §13 Q1–Q3 settled against live captures (trace
segmentation holds for TUI + exec; `promptTokens` = total input incl. cache;
assistant message is one complete read).

### Phase 1 — Trace + cost (post-hoc) — **Done**
Pricing (cache-aware, §7 carve-out), redaction, capture presets, REST ingestion
client (deterministic idempotent ids, retry, batching), trace segmentation +
builders, `trace` + `sync`. Live-verified: §14 checklist green, cost path
integrated (Langfuse derives `inputCost`/`totalCost` from our `costDetails`).

### Config slice (pulled forward from Phase 2) — **Done**
`setup` (interactive credential entry → 0600 file), `test` (connectivity probe),
`status` (masked config summary), config write path. Driven by the live-
verification need — §14 itself calls for `setup`/`test`.

### Polish — **Done**
`syncstate` cursor (sync skips sessions unchanged since last post, keyed on
`events.jsonl` mtime; `--force` bypasses). Zero-value `costDetails` components
omitted (consistent with `usageDetails`).

### Phase 2 — Watch mode — **Deferred**
`fsnotify` tailer + per-session event-sequence cursors + incremental posting +
idle/shutdown flush. **Re-trigger:** a real need for *in-flight* TUI tracing
(traces streaming while you use the Zero TUI). Post-hoc `trace`/`sync` cover
everything else (CI, replay, backfill). Building this is also when we de-risk
§13 Q4 (fsnotify reliability under Zero's `session.lock`).

> Note: incremental per-turn posting (event-sequence cursors) lands *with* watch
> mode. `sync` currently re-posts a whole session when its mtime advances —
> correct and idempotent, just coarser.

### Phase 3 — Distribution — **Planned**
GoReleaser cross-compile (linux/macos/windows × amd64/arm64), GitHub Release
assets, `install.sh`/`install.ps1`, README, optional npm wrapper, systemd/watch
hint. Gated decisions: npm publish name (§13 Q5), versioning scheme.

### Phase 4 — `exec --live` accelerator — **Optional**
Stream-json wrapper for in-flight exec visibility before a session flushes.
Lower priority than watch; only worth it if long CI exec runs need a trace row
*while still running*.

---

## Deferred / not-yet-built features (with re-triggers)

| Feature | Re-trigger | Note |
|---|---|---|
| Specialist cross-session nesting (§6.2) | Need to see child sessions as nested traces in the UI | Currently each session traces independently, linked only by `metadata.parentSessionId` |
| Permission events → tool-obs metadata (§6.2) | Need permission-denial observability | Currently ignored; tool spans carry `isError` from `tool_result.status` only |
| `flushAt`/batch-threshold tuning | Real batch-size pressure | Hardcoded 100-event chunks + per-run flush; fine at current volume |
| `reasoningTokens` billed at output rate | Per-model billing accuracy need | Currently reported in `usageDetails` but not separately priced; documented per-model |

---

## Open questions / risks (DESIGN §13)

- **Q1–Q3** — **settled** (Phase 0).
- **Q4 (watch concurrency)** — **open**, gated on Phase 2.
- **Q5 (npm publish name)** — **open**, gated on Phase 3.
- **Q6 (self-hosted smoke-test)** — **settled** (done — live-verified against self-hosted).
- **Q7 (optional upstream hook for live session id)** — **open**, nice-to-have, not required.

---

## Explicit non-goals (scope guardrails)

- **No in-process extension / no OTel / no `@langfuse/*` SDK** — REST ingestion only (DESIGN §5.2; the omp OTel bundling lesson).
- **No host/wire cost trust** — always self-compute from tokens × resolved price.
- **No modification of Zero** — the session log already has everything; no upstream fork/PR.
- **No cursor file for `trace`** — `trace` is a one-shot post; deterministic ids make it idempotent without state. Only `sync` keeps a cursor (it's the repeated backfill path).
