# zero-langfuse

Langfuse observability for [Zero](https://github.com/Gitlawb/zero), via its persisted session log. A small Go binary that reads Zero's session log and turns each user turn into a Langfuse trace — one ingestion path that serves Zero's TUI, `zero exec`, and specialist sub-agents uniformly. See [`.docs/DESIGN.md`](./.docs/DESIGN.md) for the full design.

> Status: Phase 0–1 complete and live-verified. `watch` mode (near-real-time TUI tracing, Phase 2) is deferred — post-hoc `trace`/`sync` cover everything until it lands. See [`.docs/ROADMAP.md`](./.docs/ROADMAP.md).

## Install

**One-liner (macOS / Linux):**

```bash
curl -fsSL https://raw.githubusercontent.com/nathanpt/zero-langfuse/main/scripts/install.sh | bash
```

**Windows (PowerShell):**

```powershell
irm https://raw.githubusercontent.com/nathanpt/zero-langfuse/main/scripts/install.ps1 | iex
```

**npm** (downloads the platform binary on install):

```bash
npm install -g @nathanpt/zero-langfuse
```

**From source** (requires Go; does **not** get the release version — reports `dev`):

```bash
go install github.com/nathanpt/zero-langfuse@latest
```

## Quickstart

```bash
zero-langfuse setup          # enter Langfuse host + public/secret keys (saved 0600)
zero-langfuse test           # POST one probe trace to confirm connectivity
# ...run a zero turn that uses a tool...
zero-langfuse trace --latest # post that session's traces to Langfuse
zero-langfuse sync           # backfill all sessions (idempotent upserts)
```

Shipped commands:

| Command | What it does |
|---|---|
| `dump` | Pretty-print a session's `metadata.json` + every `events.jsonl` event (Phase 0 validation tool). |
| `trace` | Build Langfuse traces for one session and POST them. One trace per user turn, a generation per `provider_usage` with cache-aware cost, tool spans, trace scores; redacted per the active privacy preset. Idempotent. |
| `sync` | Backfill all sessions under the sessions dir. Idempotent upserts, bounded by `--since`/`--limit`. |
| `setup` | Enter Langfuse host + keys interactively and save them to the config file. Flags or env vars skip the prompts. |
| `test` | POST one probe trace to confirm host + credentials work. |
| `status` | Print the resolved config (host, masked keys, privacy preset, sessions count) without uploading anything. |

Useful flags: `--latest` (act on the most recently active session), `--sessions <dir>` (override the sessions dir, default `$XDG_DATA_HOME/zero/sessions`), `--privacy <preset>` (override the privacy preset for one run), `--dry-run` (print the ingestion batch as JSON without posting — no creds needed).

## Configuration

Credentials/config live outside the repo, at `$XDG_CONFIG_HOME/zero-langfuse/config.json` (0600 in a 0700 dir). `setup` writes this file for you. You can also use env vars (which override the file):

- `LANGFUSE_BASE_URL` — Langfuse host (e.g. `https://cloud.langfuse.com` or your self-hosted URL).
- `LANGFUSE_PUBLIC_KEY` / `LANGFUSE_SECRET_KEY` — Basic-auth credentials.
- `LANGFUSE_PRIVACY_PRESET` — one of `metadata-only`, `prompts-only`, `conversations`, `full-debug`.
- `LANGFUSE_CAPTURE_*` — fine-grained capture flags applied on top of the preset.

Privacy presets (least → most data): `metadata-only` (counts/cost only), `prompts-only` (prompts, no model outputs), `conversations` (both sides, redacted), `full-debug` (everything, still redacted). See [DESIGN §8](./.docs/DESIGN.md).

## Running automatically

`watch` (near-real-time TUI tracing) is Phase 2 and **deferred**. Until it lands, the always-on path is a timer running `sync` periodically — `sync` is idempotent (trace ids derive from `(sessionId, sequence)`), so re-running it never duplicates traces.

A systemd **user** timer that re-syncs the last day hourly:

```ini
# ~/.config/systemd/user/zero-langfuse-sync.service
[Unit]
Description=zero-langfuse backfill

[Service]
Type=oneshot
# --since is a strict YYYY-MM-DD; compute yesterday in the shell. A bare
# `sync` (no --since) is also fine — it re-upserts everything idempotently.
ExecStart=/bin/sh -c 'exec zero-langfuse sync --since "$$(date -d yesterday +%%F)"'
```

```ini
# ~/.config/systemd/user/zero-langfuse-sync.timer
[Unit]
Description=zero-langfuse backfill hourly

[Timer]
OnBootSec=2min
OnUnitActiveSec=1h
Persistent=true

[Install]
WantedBy=timers.target
```

Enable with `systemctl --user daemon-reload && systemctl --user enable --now zero-langfuse-sync.timer`. (On macOS, an equivalent launchd `StartInterval` plist works the same way.) **Do not** expect a `watch` subcommand yet.

## Cost & privacy

Cost is self-computed from tokens × resolved price (cache-aware: prompt, completion, cached-input, cache-write, reasoning are all priced separately) — Zero never exposes cost on any external surface, so `zero-langfuse` computes it. See [DESIGN §7](./.docs/DESIGN.md). Redaction (API keys, bearer tokens, cookies, private keys, GitHub/npm/AWS tokens, local absolute paths) runs before anything leaves the machine; it is entirely `zero-langfuse`'s job. See [DESIGN §8](./.docs/DESIGN.md).

## License

MIT — see [LICENSE](./LICENSE).
