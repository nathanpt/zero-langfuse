# DESIGN — zero-langfuse

Langfuse observability for **[Zero](https://github.com/Gitlawb/zero)** by Gitlawb — the
"terminal coding agent you own." Sibling to [`omp-langfuse`](https://github.com/nathanpt/omp-langfuse)
(and, upstream, [`pi-langfuse`](https://github.com/gooyoung/pi-langfuse)). It reuses their
*principles* and hard-won lessons, but the host it targets forces a **different architecture**:
where omp/pi-langfuse run as in-process TS extensions subscribing to the host's lifecycle events,
**zero-langfuse reads Zero's own persisted session log** — a single, unified record that Zero writes
for both its TUI and its headless `exec` mode, including a richer token breakdown than either its
hooks or its stream-json wire expose.

> Status: design / pre-implementation, **v2** (the v1 stream-json-wrapper-centric design was
> superseded once the session log was found to carry full per-turn usage for every surface — see §4).
> Every claim about Zero's internals in §3–§5 is verified against the Zero source at commit
> `0562e3b`: `internal/sessions/store.go`, `internal/usage/report.go`, `internal/cli/exec.go`,
> `internal/cli/exec_writer.go`, `internal/tui/model.go`, `internal/specialist/accounting.go`,
> `internal/hooks/`, `internal/streamjson/streamjson.go`, `internal/modelregistry/cost.go`.

---

## 1. TL;DR

- **Name:** `zero-langfuse` (matches the `pi-langfuse → omp-langfuse → zero-langfuse` lineage;
  boring, consistent, discoverable). Recommended over `zerofuse` / `langfuse-zero`. See §2.
- **Primary design decision:** Zero already persists a complete per-turn event log
  (`events.jsonl`) for **every** run — TUI, `zero exec`, and specialist sub-agents — including a
  token breakdown with cache + reasoning fields. **zero-langfuse reads that log** and turns it into
  Langfuse traces. One ingestion path serves all surfaces; no upstream contribution required.
- **Shape:** a small **Go binary** that watches/reads the session log and POSTs traces to Langfuse
  via the **REST ingestion API** (no OTel/SDK — §6). Two modes: `watch` (tail active sessions,
  near-real-time — the daily-driver path for both TUI and exec) and `trace`/`sync` (post-hoc read of
  one or all sessions — the CI/replay/backfill path).
- **Fidelity:** one trace per user turn (response), grouped by Zero session, with a generation per
  model turn carrying **cache-aware, self-computed cost**, plus tool/permission/error observations
  and nested specialist traces. Achievable **uniformly** for TUI and exec.

---

## 2. Goals & Non-Goals

### Goals
- Ship **one Langfuse trace per user turn** (one per assistant response), grouped into a Langfuse
  **session** per Zero session — for **both the TUI and `zero exec`**.
- Capture, per trace: a generation per model turn with token usage + **cost we compute ourselves**
  (cache-aware), one observation per tool call (with error detection), permission events, and
  specialist sub-runs as nested traces.
- Feel native to Zero: a Go binary on PATH, configured via XDG + `LANGFUSE_*` env, invokable as
  `zero-langfuse watch` (always-on) or `zero-langfuse trace <session>` (one-shot / CI).
- Reuse omp-langfuse's proven, host-agnostic logic: pricing table + resolution, redaction patterns,
  privacy presets, trace-level scores, the "never trust host cost" rule.

### Non-Goals (v1)
- No in-process Zero extension. Zero is a compiled Go binary with no JS/code-plugin host; we observe
  it from outside via its own persisted state.
- No OTel / `@langfuse/tracing` / `@langfuse/otel`. REST ingestion only (§6).
- No support for non-Zero hosts.
- No modification of Zero itself (an upstream `usage` hook is no longer required — the session log
  already has usage; see §13 for the one place an upstream change would still help).

---

## 3. Background: Zero vs OMP/Pi

| | OMP / Pi | Zero |
|---|---|---|
| Runtime | Bun/Node, TS extension host | Compiled Go binary, no code-plugin host |
| Extension surface | In-process: `pi.on("before_provider_request", …)` | Out-of-process: **hooks** (shell cmds, JSON on stdin) + **plugins** + **MCP** + **specialists** |
| Where per-turn usage lives | `message.usage` event, in-process | **Persisted to disk** in the session log (`events.jsonl`) — readable by any process |
| Cost | Host zeroes it; we self-compute | Host never exposes it on any external surface; we self-compute (§7) |
| Integration point | An extension bundle loaded by the host | A **sidecar reader** of the host's session store |

Zero is open-source (MIT), CLI-first, distributed as a platform binary via GitHub Releases with an
optional npm wrapper (`@gitlawb/zero`). Config layers: built-in → `~/.config/zero/config.json`
(user) → `./.zero/config.json` (project) → CLI flags → `ZERO_*` env. Sessions persist under
`$XDG_DATA_HOME/zero/sessions/`.

---

## 4. The Decisive Finding — Zero Persists Everything We Need, For Every Surface

This is the core of the design and the thing v1 got wrong. v1 assumed the only usage surface was the
`zero exec --output-format stream-json` wire (and thus that the TUI was usage-blind). The source says
otherwise.

### 4.1 The session log is the source of truth
Zero's session store (`internal/sessions/store.go`) writes, per session, an append-only JSONL of
**events** plus a metadata file:

```
$XDG_DATA_HOME/zero/sessions/<sessionId>/
├── metadata.json     # sessionId, modelId, provider, cwd, title, parentSessionId, rootSessionId, …
└── events.jsonl      # one JSON event per line, fsync'd on append (durable, safe to tail)
```

Verified persisted event types (`sessions/store.go:26-46`):

```
message · tool_call · tool_result · permission · permission_request · permission_decision
provider_usage (= usage) · error · session_checkpoint · session_rewind · session_compaction
session_fork · session_child · specialist_start · specialist_stop · spec_*
```

This is a **complete trace feed**: prompt/answer messages, every tool call + result, every permission
gate, per-turn usage, errors, and the specialist/compaction/checkpoint lifecycle — strictly richer
than the hooks surface **and** richer than the stream-json wire.

### 4.2 Per-turn usage is persisted WITH cache + reasoning breakdown — by every surface
The usage payload (`internal/usage/report.go:21-29`) is:

```go
type usageEventPayload struct {
    PromptTokens      int    `json:"promptTokens"`
    CompletionTokens  int    `json:"completionTokens"`
    TotalTokens       int    `json:"totalTokens"`
    CachedInputTokens int    `json:"cachedInputTokens,omitempty"`  // cache READ — enables cache-discount cost
    CacheWriteTokens  int    `json:"cacheWriteTokens,omitempty"`   // cache WRITE — enables cache-write premium
    ReasoningTokens   int    `json:"reasoningTokens,omitempty"`
    Model             string `json:"model,omitempty"`              // escalation runs only
}
```

And it is written by **all three** run surfaces (each verified in source):

| Surface | Append site | Verified |
|---|---|---|
| `zero exec` | `internal/cli/exec.go:583` `sessionRecorder.append(sessions.EventUsage, payload)` | ✅ |
| TUI | `internal/tui/model.go:4496-4497` `Type: sessions.EventUsage, Payload: usage.EventUsagePayload(event)` | ✅ |
| Specialists | `internal/specialist/accounting.go:105` `appendSpecialistEventOnce(… sessions.EventUsage …)` | ✅ |

Zero itself already reconstructs cache-aware cost from this log — `usage.BuildReport` (report.go:89)
prices each turn "exactly (cache discount + cache-write premium + reasoning) rather than estimating
from prompt/completion alone" via `modelregistry.CalculateCost`. We do the same.

### 4.3 Contrast: what hooks and stream-json give us (and why they're not the primary path)
- **Hooks** (`internal/hooks/`): fire only on `beforeTool`/`afterTool`/`session{Start,End}`/
  `specialist{Start,Stop}`. Verified payloads (`agent/loop.go:1413,1436`):
  `beforeTool = {event,tool,toolCallId,sessionId,cwd,args}`, `afterTool = {…,status,changedFiles}`.
  **Zero references to tokens/usage/cost/model in `internal/hooks`: none.** Hooks cannot produce a
  generation or a cost figure. → Not the primary path.
- **stream-json wire** (`internal/cli/exec_writer.go:261-272`): the `usage` event emits **only**
  `promptTokens/completionTokens/totalTokens`. The struct's `CostUSD` field is **never populated**;
  there is **no cache/reasoning breakdown** on the wire. → Reachable only from `zero exec`, and
  lower-fidelity than the session log.

### 4.4 Conclusion
**The session log is the unified, authoritative, highest-fidelity source — available for TUI, exec,
and specialists alike.** Reading it is the primary architecture. Hooks are unnecessary; stream-json
is an optional exec-only accelerator (§10). This is what makes "design for both TUI and exec" not
just possible but uniform.

---

## 5. Architecture — Session-Log Reader (Go, REST-first)

### 5.1 Why a reader of persisted state (not a wrapper, not an in-process extension)
omp-langfuse runs inside the host and gets called by lifecycle events. Zero has no such surface.
v1 proposed wrapping `zero exec` and parsing stream-json — but that served only exec and at lower
fidelity. The session log is better on both axes: it covers every surface and carries the cache
breakdown. So zero-langfuse is a **reader** of the session store, not a wrapper of the agent.

### 5.2 Why REST-first (the omp-langfuse lesson #11, carried forward)
omp-langfuse's single hardest bug was that OMP's Bun runtime could not resolve the OTel dependency
graph from `node_modules` (`Cannot find module '@opentelemetry/core'`), fixed only by esbuild-
bundling the entire extension. omp-langfuse also carries a REST fallback because self-hosted Langfuse
sometimes accepts OTel spans but never materializes a trace. An out-of-process reader has zero reason
to use OTel. → **zero-langfuse is REST-native from day one**: direct `POST /api/public/ingestion`
to Langfuse (Basic auth `pk-lf-…:sk-lf-…`), batched + retried + flushed. No OTel, no `@langfuse/*`,
no Node runtime. The single largest simplification vs omp-langfuse.

### 5.3 Why Go
- **Single static binary, no runtime dependency** — an always-on watcher must not require Node/Bun.
- **Matches Zero's ecosystem** (Zero is Go; users already trust a Go binary on PATH).
- **Trivial cross-platform release** via GoReleaser, mirroring Zero's own `release-artifacts.yml`.
- First-class file-tail (`fsnotify`), `encoding/json` line parsing, and a tiny HTTP client.

(Node/TS was considered for direct reuse of omp-langfuse's pricing/redaction; rejected: re-adds a
runtime dependency for a sidecar, and the OTel SDK is in-process-oriented and useless here. We port
the *logic*, not the code — the same posture omp took toward pi-langfuse.)

### 5.4 Two runtime modes
```
zero-langfuse watch [--sessions <dir>]      # PRIMARY — daily driver, TUI + exec
   tails $XDG_DATA_HOME/zero/sessions/**/*.jsonl with fsnotify
   per active session: segment events into traces, post incrementally (batched REST)
   groups traces by sessionId → Langfuse session
   captures every run that writes to the store (TUI, exec, specialists)

zero-langfuse trace <sessionId|--latest>    # ONE-SHOT — CI / replay
   reads one completed session, builds + posts all its traces, exits

zero-langfuse sync [--since <date>]         # BACKFILL
   walks the session dir, posts traces for sessions not yet in Langfuse (idempotent by trace id)
```

- **`watch`** is the always-on path: start it once (login shell, systemd user unit, or alongside
  `zero`) and every Zero run — interactive TUI included — is traced near-real-time. The session log
  is fsync'd on every append (`sessions/store.go` `writeFileSync` / `appendEventLocked`), so tailing
  is safe and low-latency.
- **`trace`** is the CI path: `zero exec … ; zero-langfuse trace --latest` (or the wrapper
  `zero-langfuse exec …` that runs exec then traces — §10). Deterministic, exit-coded, replayable.
- **`sync`** backfills history and recovers after Langfuse/outage gaps.

### 5.5 Isolation & correlation
One session = one `events.jsonl` = one Langfuse **session**. Each reader process is stateless beyond
a per-session append cursor (last sequence number seen), so multiple watchers or a crash-then-restart
simply resume from the cursor — no omp-style `AsyncLocalStorage` needed. Specialist child sessions
link to parents via `metadata.parentSessionId`/`rootSessionId` → nested traces in the same Langfuse
session.

---

## 6. Trace Model & Session-Event → Observation Mapping

Same three-tier model as omp/pi-langfuse (trace / generation / tool), driven by session events.

### 6.1 Trace segmentation (one trace per user turn)
A Zero session is a long-lived append-only conversation (the TUI is one session across many turns; a
resumed `exec` appends to an existing session). So a session log is segmented into traces by **user
turn boundaries**: a user `message` event opens a new trace; subsequent events (assistant message,
tool_call/tool_result pairs, permission, usage, errors) attach to it until the next user `message`
(or session end). This yields exactly **one trace per response**, bundled by session — for both TUI
and exec.

### 6.2 Event → observation map
```
Trace  (name: "zero-agent", id: <sessionId>#<turnSeq>)
├── sessionId : metadata.sessionId                      # Langfuse session grouping
├── input     : user message payload
├── output    : assistant message payload (final)
├── metadata  : { provider, modelId, cwd, title, rootSessionId } from metadata.json  (privacy-gated)
├── Generation obs (name: "llm-generation")              ← one per provider_usage event
│   ├── model        : metadata.modelId (or payload.model on escalation)
│   ├── usageDetails : { input, output, total, cachedInput, cacheWrite, reasoning }
│   └── costDetails  : { input, cachedInput, cacheWrite, output, total }  ← SELF-COMPUTED (§7)
├── Tool obs (name: <tool>)                              ← per tool_call + tool_result (matched by id)
│   ├── input  : tool_call payload
│   ├── output : tool_result payload (truncated)
│   └── metadata: { toolCallId, status, isError = (status != "ok"), changedFiles }
├── Permission events                                    ← metadata on the gated tool observation
└── Error event                                          ← trace/obs level=ERROR, status=ERROR
```

Specialist sub-runs (`session_child` + the child session's own log) → a nested trace in the same
Langfuse session, linked via `parentSessionId`. Compaction/checkpoint/rewind events → trace metadata
(observability of context management, not observations).

### 6.3 Robustness (omp lesson, restated for an append-only log)
- Tail from a persisted per-session **cursor** (last sequence); replay-safe and crash-safe.
- Skip + warn on malformed lines rather than aborting the trace (a torn trailing line is normal —
  `sessions` itself ignores it via `lastEventSequence`).
- Tolerate events arriving without an opener (orphan `tool_result`/`usage`) by self-closing.
- Always flush on session-idle / shutdown / EOF.
- **Idempotency:** trace/observation ids are derived from `(sessionId, sequence)` so `sync`/re-run
  never duplicates in Langfuse.

**Trace-level scores (carry-over from omp-langfuse):** `tool_call_count`, `total_tool_errors`,
`tool_success_rate` (0–1), `turn_had_errors` (0/1), `generation_count`, and `cache_hit_rate`
(cachedInput / prompt tokens — free, since we have the breakdown).

---

## 7. Pricing & Cost (self-computed, cache-aware, REST-only)

Mirrors omp-langfuse §8.1 / `src/pricing.ts`, ported to Go, **now cache-aware** because the session
log carries `cachedInputTokens`/`cacheWriteTokens`/`reasoningTokens`:

```
uncached        =  promptTokens - cachedInputTokens - cacheWriteTokens
cost.input      =  uncached          * rate.input      / 1e6
cost.cachedInput=  cachedInputTokens * rate.cacheRead   / 1e6
cost.cacheWrite =  cacheWriteTokens  * rate.cacheWrite  / 1e6
cost.output     =  completionTokens  * rate.output      / 1e6
cost.total      = sum of the above
```

`promptTokens` is the **total** input (uncached + cache-read + cache-write — `zeroruntime.Usage`
doc, types.go:127), so **both** cache subsets must be subtracted from the input pool. Confirmed
against a live z.ai/glm-5.2 capture (promptTokens 8149 = uncached 21 + cacheRead 8128) and against
Zero's own `modelregistry.CalculateCost` (cost.go:91), which computes `uncached = input −
cachedInput − cacheWrite`. An earlier draft of this section subtracted only `cachedInputTokens`,
which double-counted `cacheWriteTokens` for Anthropic models (cache-write priced at both the input
rate and the cache-write premium).

Rates stored **per-million-tokens** (matches how providers publish), divided by `1e6` at compute
time. `reasoningTokens` are typically billed at the output rate (provider-dependent; document per
model). Implementation note (matching Zero, cost.go:80-94): only carve a cache subset out of the
input pool when its rate is > 0 — models priced without a cache-read/cache-write rate leave those
tokens billed at the full input rate. Compare to v1 (stream-json only), which could not price cache
separately and over-stated input cost for cache-heavy runs — **the session log fixes this**.

**Price resolution precedence** (first match wins), resolved per `metadata.modelId` (or
`payload.model` on escalation):

1. **User config override** — exact model-id match in `config.pricing` (§8).
2. **Bundled table, exact** model-id match.
3. **Bundled table, family prefix** — longest matching family key (e.g.
   `claude-sonnet-4-20250514` → `claude-sonnet-4`; `glm-5.2` → `glm` family → GLM-4.6 estimate).
4. **None** — omit `costDetails`; emit a one-time `zero-langfuse: no price for model "<id>"` warning.

> **Carry-over guardrail:** omp-langfuse hit a historical off-by-1e6 because the OMP *catalog* rate
> was already $/Mtok and got converted again. Zero has no catalog rate on any external surface, so
> this precedence has **no** registry/catalog step — only config + bundled table. (Zero's own
> `modelregistry.CalculateCost` is internal to the binary and unreachable to us; we recompute, as
> omp did.)

**Bundled starter table** is shared with omp-langfuse (per-Mtok, USD), keyed to families Zero
supports (OpenAI, Anthropic, Gemini, OpenRouter, DeepSeek, Qwen/Kimi, GLM; local Ollama/LM-Studio →
0). Maintenance reality (omp §8.1): pricing drifts; the one-time "no price" warning + trivial config
overrides are the correction path.

---

## 8. Privacy, Redaction & Capture Presets

### 8.1 Carry-over from omp-langfuse
- **Presets:** `metadata-only` → `prompts-only` → `conversations` → `full-debug` (default), plus the
  fine-grained flags (`LANGFUSE_CAPTURE_INPUTS`/`_OUTPUTS`/`_TOOL_IO`/`_CWD`) overriding the preset.
- **Redaction** (port omp `src/redaction.ts` patterns): API keys, bearer tokens, passwords, cookies,
  private keys, Langfuse keys, GitHub/npm/AWS-style tokens, and **local absolute paths** — masked
  before upload.

### 8.2 Zero-specific notes
- The session log is **un-redacted on disk** (it is Zero's private transcript); redaction is entirely
  our responsibility before upload. (Contrast: Zero's stream-json output *is* redacted at emission —
  one more reason the log is the richer source.)
- `metadata.json` carries `cwd` and git-derived identity candidates — gate `cwd` behind the
  `conversations`+ preset / `_CWD` flag, as omp does.

---

## 9. Configuration & Credentials

### 9.1 Credential resolution (precedence high → low)
1. **Env vars:** `LANGFUSE_PUBLIC_KEY`, `LANGFUSE_SECRET_KEY`, `LANGFUSE_BASE_URL`
   (default `https://cloud.langfuse.com`; alias `LANGFUSE_HOST`).
2. **Config file:** `$XDG_CONFIG_HOME/zero-langfuse/config.json` (→
   `~/.config/zero-langfuse/config.json`), mode `0600` in a `0700` dir. Written by `zero-langfuse setup`.

```jsonc
{
  "publicKey": "pk-lf-...",
  "secretKey": "sk-lf-...",
  "host": "https://cloud.langfuse.com",
  "privacy": "full-debug",
  "sessionsDir": null,            // null → $XDG_DATA_HOME/zero/sessions (auto-discovered)
  "flushAt": 5,                   // batch threshold (events)
  "flushIntervalMs": 1000,
  "pricing": {                    // per-Mtok USD overrides (§7); omitted components default to 0
    "glm-5.2":         { "input": 0.50, "output": 2.00, "cacheRead": 0.10 },
    "claude-sonnet-4": { "input": 3, "output": 15, "cacheRead": 0.30, "cacheWrite": 3.75 }
  }
}
```

### 9.2 Subcommands
- `zero-langfuse watch` — tail active sessions, post traces near-real-time (primary; TUI + exec).
- `zero-langfuse trace <id|--latest>` — post-hoc read of one session (CI / replay).
- `zero-langfuse sync [--since <date>]` — backfill all sessions (idempotent).
- `zero-langfuse exec [zero flags] "prompt"` — convenience: run `zero exec`, then `trace --latest`.
- `zero-langfuse setup` / `status` / `test` — key entry, masked status, connectivity probe.

---

## 10. The `zero exec` Accelerator (optional, exec-only)

For long CI runs that want **in-flight** Langfuse visibility before a session file is fully flushed,
ship an optional stream-json wrapper:

```
zero-langfuse exec --live [zero flags] "prompt"
   spawns:  zero exec --output-format stream-json [flags] "prompt"
   echoes:  the stream faithfully to stdout (CI consumers unaffected — transparent tee)
   traces:  posts an in-flight trace from the stream (prompt/completion/total tokens → cost)
   then:    reconciles final cost from the session log (cache breakdown) on run_end
```

Lower priority than `watch`/`trace`: the session-log watcher already covers exec near-real-time, so
`--live` is purely for "I need the trace row in Langfuse while the 10-minute exec is still running."
Cost from the stream is cache-blind; the post-run reconciliation with the log makes it cache-aware.

---

## 11. Distribution

Mirrors Zero's own model (Go binary via GitHub Releases + optional npm wrapper), **not** the omp
git-source-install model:

- **Primary:** GoReleaser cross-compile → GitHub Release assets (linux/macos/windows × amd64/arm64),
  plus `install.sh` / `install.ps1`. Users put `zero-langfuse` next to `zero` on PATH.
- **Optional npm wrapper:** `zero-langfuse` on npm, a thin postinstall that fetches the platform
  binary (mirrors `@gitlawb/zero`'s own wrapper). Publish name to confirm (§13).
- **Always-on hint:** document a systemd user unit / login-shell line for `zero-langfuse watch`.
- CI: build + test on PRs; on `v*` tags, GoReleaser + GitHub Release. No bundle to commit (unlike
  omp-langfuse's `dist/index.js`) — a Go binary has no in-tree build artifact.

---

## 12. Carry-Overs vs New-for-Zero (explicit ledger)

| Principle / component | omp-langfuse source | zero-langfuse |
|---|---|---|
| Never trust host/wire cost; always self-compute | §8.1, `src/pricing.ts` | **Reused** — now cache-aware from the session log (§7) |
| REST ingestion as the reliable path | `src/langfuse.ts` REST fallback | **Promoted to primary & only** (§5.2) |
| Pricing table + resolution (config → exact → family → none+warn) | §8.1 | **Reused**, minus the catalog step |
| Privacy presets + secret/path redaction | `src/capture-policy.ts`, `src/redaction.ts` | **Reused** (log is un-redacted on disk → 100% our job) |
| Three-tier trace model + trace-level scores | §5.1, `src/handlers/*` | **Reused**; events come from the session log, not lifecycle hooks |
| Robustness to late/out-of-order/teared events | §10 live finding | **Restated** for an append-only, fsync'd, cursor-tailed log |
| Session isolation via `AsyncLocalStorage` | `src/state.ts` | **Dropped** — stateless per-session cursor replaces it |
| Bundled `dist/` artifact committed | omp critical constraint #2 | **Dropped** — Go binary release |
| In-process extension bundle (esbuild + Bun fix #11) | `index.ts` + `dist/` | **Dropped entirely** — out-of-process log reader |
| Trace input = full provider request body | `before_provider_request.payload` | **Reduced** — we get the user/assistant *messages*, not the raw request body (no system prompt, no tools schema). Documented fidelity ceiling. |
| **NEW** cache-aware cost (cachedInput/cacheWrite) | partial (OMP `message.usage` had cache fields) | **Full**, from `usageEventPayload` cache fields |
| **NEW** nested specialist traces from `session_child` | n/a (OMP specialists differed) | First-class, via child session logs |

---

## 13. Open Questions / Risks

1. **Trace-segmentation correctness (HIGH PRIORITY, Phase 0).** "One trace per user turn" depends on
   every run appending a user `message` event before its assistant work. Verified for exec
   (`exec.go:485`); **confirm for the TUI** at runtime — if the TUI batches or omits the user-message
   marker, segmentation needs an alternative boundary (e.g. assistant-message starts). Resolve with a
   live TUI session capture.
2. **`EffectiveInputTokens()` cache semantics.** The persisted `promptTokens` =
   `usage.EffectiveInputTokens()`; confirm whether that *includes* or *excludes* cached input, since
   it determines whether our `(promptTokens − cachedInputTokens)` subtraction double-counts. The
   method def was not reachable by grep in this checkout — verify live. (If `promptTokens` already
   excludes cache, drop the subtraction.)
3. **`final`/assistant-message completeness.** Confirm the assistant `message` payload always
   carries the full answer text (so trace output is one read, not delta reassembly).
4. **Watch-mode concurrency.** Confirm `fsnotify` reliably catches appends under Zero's
   cross-process file lock (`session.lock`) and that tailing never races an in-flight fsync (the
   store ignores torn trailing lines; the reader must too).
5. **npm publish name** — `zero-langfuse` (assumed) vs a scope; confirm ownership.
6. **Self-hosted Langfuse** — REST-only ingestion is known to work against self-hosted (omp's
   fallback); smoke-test before release.
7. **Optional upstream ask (nice-to-have, NOT required):** a Zero hook event carrying the *live*
   session id at run start would let `watch` skip the fsnotify setup latency for the very first
   trace of a session. Not needed for correctness.

---

## 14. Validation Plan (before merge of any trace-path change)

Unit tests alone are not enough (omp lesson). Validate against a real Langfuse:

1. `go build` + `go test ./...`.
2. **TUI path:** start `zero-langfuse watch`, run an interactive `zero` turn that uses a tool and
   one that fails — confirm in Langfuse: one trace per turn, generation `usageDetails` with cache
   fields + populated `costDetails`, tool observations with `isError` on the failure, trace scores,
   and correct session grouping across turns.
3. **exec path:** `zero exec … ; zero-langfuse trace --latest` — confirm the same trace shape, plus
   exit-code parity with bare `zero exec`.
4. **Backfill idempotency:** run `sync` twice; confirm no duplicate traces (ids derived from
   `(sessionId, sequence)`).
5. `zero-langfuse test` against both Langfuse Cloud and a self-hosted instance.

---

## 15. Implementation Phases

- **Phase 0 — Scaffold & validate the session log.** Go module; a `dump` command that reads a
   session's `events.jsonl` + `metadata.json` and pretty-prints every event type with its payload.
   Resolve §13 Q1/Q2/Q3 against live TUI + exec captures. (This de-risks the entire design.)
- **Phase 1 — Trace + cost (post-hoc).** REST ingestion client (batched + flush), pricing module
  (§7), trace segmentation (§6.1), generation/tool/error builders, self-computed cache-aware cost,
  trace scores, idempotent ids. `trace` + `sync` commands.
- **Phase 2 — Watch mode + privacy.** `fsnotify` tailer with per-session cursors, incremental
  posting, idle/shutdown flush. Capture presets + redaction (§8), `setup`/`status`/`test`.
- **Phase 3 — Distribution.** GoReleaser + GitHub Release assets, install scripts, optional npm
  wrapper, README, systemd/watch hint.
- **Phase 4 (optional) — exec live wrapper.** The `--live` stream-json accelerator (§10), gated on
  real demand for in-flight exec visibility.

---

## 16. References

- Zero repo: https://github.com/Gitlawb/zero
- Zero extension guide: `AGENTS.md` (hooks, plugins, MCP, specialists, skills, config layers)
- Zero stream-JSON protocol: `docs/STREAM_JSON_PROTOCOL.md`
- Zero source (verified): `internal/sessions/store.go` (event types, `DefaultRoot`, durable append),
  `internal/usage/report.go` (`usageEventPayload`, `EventUsagePayload`, `BuildReport`),
  `internal/cli/exec.go` (`OnUsage`→session append), `internal/cli/exec_writer.go` (stream-json wire),
  `internal/tui/model.go` (TUI usage append), `internal/specialist/accounting.go` (specialist usage),
  `internal/hooks/` + `internal/agent/loop.go` (hook payloads), `internal/modelregistry/cost.go`
- Sibling project (carry-over source): https://github.com/nathanpt/omp-langfuse — esp. `.docs/DESIGN.md`
  §5 (trace model), §8.1 (cost), §10 #11 (Bun/OTel bundling — the lesson driving §5.2 here)
- Upstream of that: https://github.com/gooyoung/pi-langfuse (v1.5.6)
- Langfuse public ingestion API: `POST /api/public/ingestion` (batched `trace-create` /
  `generation-create` / `score-create`, Basic auth `pk-lf-…:sk-lf-…`)
