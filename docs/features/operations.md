# Operations & Deployment

> Cost tracking, benchmark, sync, auto-update, scheduler, caps, sandbox, differentiators

## 1. Cost Tracking & Rate-Limit Awareness

### Budget Tracking
- `daily_spend` table tracks costs per bucket per day
- **Extraction buckets:** `extract`, `quality` — LLM costs for Haiku/Sonnet extraction, evolution, narratives
- **Proxy buckets:** `proxy_input`, `proxy_output`, `proxy_cache_read`, `proxy_cache_write` — token counts from Claude Code sessions (fire-and-forget RPC after each API response)
- Separation allows independent cost analysis: what YesMem's automation costs vs. what the user's sessions consume
- `yesmem cost` — CLI shows daily spend history with totals
- **Persistent** — survives daemon restarts (restored from DB)
- **Per-bucket limits:**
  - `daily_budget_extract_usd: 20.0` (extraction + evolution)
  - `daily_budget_quality_usd: 10.0` (narrative + quality)
- 80% warning logged when approaching limit

### Rate-Limit Awareness

Built-in backoff to share API quota with Claude Code sessions:

| Level | Delay | On Rate Limit |
|---|---|---|
| Between sessions | yield (`runtime.Gosched`) | 30s backoff |
| Between chunks | 2s | 60s backoff |
| Evolution checks | 5s | 60s backoff |

### Rate-Limit Header Tracking (v1.0.1)

The proxy parses Anthropic rate-limit headers from every API response:
- **`ParseRateLimitHeaders`** extracts `anthropic-ratelimit-requests-*` and `anthropic-ratelimit-tokens-*` headers into a `RateLimitInfo` struct
- **`ShouldThrottle`** with fallback chain: header-based utilization → estimated utilization → conservative default
- **Extraction throttle:** When API utilization exceeds 50%, the extraction pipeline pauses to preserve quota for interactive sessions
- **`_track_usage` RPC** accepts `cache_read`, `cache_write`, and rate-limit fields alongside standard token counts
- **Schema v0.52:** `token_usage` table has `cache_read` and `cache_write` columns for granular cache hit/miss tracking

---

## 2. LoCoMo Benchmark (E5a)

Reproducible benchmark against the LoCoMo dataset (Long Conversation Memory) — the de-facto standard for memory system evaluation.

### Pipeline

```
locomo10.json → Ingest → Extract → Query → Judge → Report
```

| Stage | What | Tool |
|-------|------|------|
| **Ingest** | 10 conversations → YesMem sessions (Speaker A=User, B=Assistant, original timestamps) | `yesmem locomo-bench ingest` |
| **Extract** | Standard 4-pass extraction pipeline per conversation | `yesmem locomo-bench extract` |
| **Query** | `hybrid_search` per question, Top-K learnings as context → LLM answers | `yesmem locomo-bench query` |
| **Judge** | LLM judge scores Generated vs. Gold answer | `yesmem locomo-bench judge` |
| **Report** | Aggregate per category + overall mean (categories 1-4, adversarial excluded) | `yesmem locomo-bench report` |

### QA Categories

| Category | ID | What it measures |
|----------|----|------------------|
| Single-hop | 1 | Direct fact retrieval |
| Multi-hop | 2 | Reasoning across multiple facts |
| Temporal | 3 | Time-based questions |
| Open-domain | 4 | World knowledge + memory combined |
| Adversarial | 5 | **NOT scored** — trick questions |

### Key Results

- **Score progression:** 0.13 → 0.62 through 5 optimizations (embedding fix, anticipated queries 3→5, BM25 optimizations, tiered search, dataset correction)
- **anticipated_queries as separate vectors** was the breakthrough: multi-hop jumped from 0.11 to 0.62
- Own extraction (0.47) beats gold observations (0.41) because gold has no aq-vectors
- Agentic mode (LLM uses YesMem tools directly): 0.69 on 10% sample
- **Tool rotation in agentic mode:** Rounds 0–2 force specific search tools (hybrid → deep → keyword) to ensure coverage before letting the model choose freely

### CLI

```bash
yesmem locomo-bench run --data ./locomo10.json --eval-llm gpt-5.4-mini
yesmem locomo-bench ingest --data ./locomo10.json    # step-by-step
yesmem locomo-bench run --eval-llm sonnet             # full pipeline
```

The subcommand is required: `yesmem locomo-bench --data …` without one is read as an unknown subcommand (`os.Args[2]` is the subcommand slot, so the flag lands there).

Note: Individual subcommands (`extract`, `query`, `judge`, `report`) are recognized but not yet wired — use `run` for the full pipeline.

### Variants

| Variant | What | Shows |
|---------|------|-------|
| A: Learnings only | Extraction → hybrid_search → answer | Learning quality |
| B: Learnings + Raw Messages | hybrid_search + FTS5 on messages | Two-layer advantage |
| C: Full-Context Baseline | Entire conversation → LLM directly | Baseline without memory |
| D: Proxy + Compression | Conversation through proxy → QA | Compression preserves quality? |

---

## 3. Sync-Public Script

The `scripts/sync-public.sh` script synchronizes the private repository to the public GitHub mirror. It works with an **exclude list** plus an explicit **allowlist for `docs/`** (rsync filter rules): everything is mirrored except the excluded paths, and under `docs/` only the named files are copied — including `docs/images/**` **and the whole `docs/features/` tree** (`+ docs/features/`, `+ docs/features/**`). There is no whitelist mode that would require listing every file explicitly, and `docs/features/` must be assumed **public**: do not put unpublished material there.
- **`--branch` flag**: sync a specific branch instead of main
- **`--per-commit` mode**: replays commits individually instead of squashing into one
- **Auto-CHANGELOG generation**: generates CHANGELOG entries from git log automatically

## 4. Auto-Update

Self-updating binary distribution via GitHub Releases — no Git required on target systems.

### Update Pipeline

1. **Check** — `GET /repos/carsteneu/yesmem/releases/latest`, semver comparison with `buildinfo.Version`
2. **Download** — platform-specific archive (`yesmem_{os}_{arch}.tar.gz`) with SHA256 checksum verification against `checksums.txt`
3. **Replace** — atomic swap: write to temp file → `os.Rename` to target path. Old binary preserved as `.bak`
4. **Migrate** — `yesmem migrate` runs DB schema migration, directory creation, config merge (new defaults without overwriting user values), hook updates
5. **Restart** — `yesmem restart` restarts daemon + proxy with the new binary

### Safety

- **SHA256 checksum verification** — archive validated before extraction
- **io.LimitReader** (200 MB) — prevents memory exhaustion from malicious payloads
- **Extraction guard** — daemon skips update while extraction or indexing is running (atomic `extractionActive` flag)
- **Atomic binary swap** — temp file + rename, never partial writes to live binary
- **Backup** — old binary always preserved as `yesmem.bak` for manual rollback
- **exec timeouts** — migrate and restart commands have 20s timeout via `context.WithTimeout`
- **Non-semver handling** — commit hashes (e.g. `7ba6267`) always trigger update offer

### Daemon Integration

- Periodic ticker (configurable via `update.check_interval`, default `6h`)
- Respects `update.auto_update` config flag (default: `true`)
- Skips check when extraction or indexing is active
- Logs all update activity: check, download, install, restart

### CLI Commands

| Command | Description |
|---------|-------------|
| `check-update` | Check if a newer version is available on GitHub Releases |
| `update` | Download, verify, install, migrate, and restart |
| `migrate` | Post-update migration: DB schema, directories, config merge, hooks |

### Configuration

```yaml
update:
  auto_update: true        # periodic daemon checks (default: true)
  check_interval: "6h"     # how often to check (default: 6h)
  channel: "stable"        # release channel (future: beta)
```

---

## 5. Capabilities System (Cap-Spec v1.1)

YesMem capabilities are reusable, tested tool definitions persisted in `caps.db` and rendered as Markdown bundles (`CAP.md`). The cap system answers a recurring need: "I built this useful tool inside one Claude session and want to call it from any future session, from a scheduled job, or from the proxy mid-fork."

### Cap Anatomy

A `CAP.md` file declares one or more **scripts**, each with a runtime and a kind:

| Field | Meaning |
|-------|---------|
| `kind: tool` | Registers as a callable tool inside the Claude REPL via `registerTool()` |
| `kind: handler` | Server-side handler invoked by another script or by the scheduler |
| `runtime: repl` | Body is JavaScript executed in the Claude Code REPL VM (sealed allowlist, no `import`/`require`) |
| `runtime: bash` | Body is a Bash script executed by the daemon (with optional sandbox profile) |
| `sandbox: none/standard/strict` | Per-script override of the job-level sandbox profile; `none` is rejected on `scope: project` caps |
| `databaseSQL` | DDL block describing tables this cap maintains via `cap_store` |

### Lifecycle

1. **Author** — write `CAP.md` (typically via the `yesmem-cap-builder` bundled skill, which ships the spec, recipes, an API reference, and 28 documented gotchas).
2. **Save** — call `save_cap` (MCP). The daemon parses the file via `internal/capfile`, validates scripts/runtimes/sandbox values, hydrates `databaseSQL` via `GetCapTableDDL`, and writes a row to `caps.db` with an auto-incrementing version.
3. **Activate** — `activate_cap` returns `registerTool()` JS the REPL evaluates. From then on, the cap appears as a native MCP-style tool in that thread; subsequent threads re-inject it automatically.
4. **Run** — either interactively (REPL invokes the registered tool) or via the scheduler (`mode=bash`, `cap_name=…`, `script_name=…`).
5. **Auto-correct** — failed bash runs are diff-classified by Sonnet and either auto-applied via `save_cap` or surfaced as `cap_proposed` rows for user review (see §20 Bash Mode + Auto-Correct).

### Bundled Caps

`caps/bundled-caps/<name>/CAP.md` is embedded into the binary. The installer `InstallBundledCaps` (`internal/setup/setup.go`) writes the bundled body into the configured caps dir, `~/.claude/caps` by default; it runs on `yesmem setup`, on `yesmem migrate` and, once an update has installed a release, through `yesmem update`. `make deploy` rolls out no cap at all: it builds the binary, swaps `~/.local/bin/yesmem`, copies the injector and the plugins and restarts the services (measured 2026-10-23: the rocketchat cap stood at v15 before and after a deploy).

The installer decides from provenance, not from the revision (`installBundledCaps`, `internal/setup/setup.go`). The provenance lives in `manifest.yaml`, which ships as `caps/bundled-caps/manifest.yaml` and exists again as `<caps_dir>/manifest.yaml` next to the installed caps. One entry per cap, and it is a claim about one body: `derived_from_hash` is the digest of the CAP.md the bundle was cut from, `revision` counts the installs that reached that installation. The digest is `textutil.BodyDigest`: line endings and trailing newlines are normalized, everything else is compared byte for byte, because inside a bash or python body the case and the indentation are the program. The frontmatter `version:` is display only, it gates nothing.

Three cases decide an install, and the installation's own manifest wins over the bundle's claim, because it records what that installation actually took. *(1) No entry.* Neither file names a body. A cap that sits nowhere is installed and tracked from that body on; a cap that already has a body is kept and reported. *(2) The provenance agrees with the file on disk.* The bundled body hashing like the installed body means there is nothing to write and only the mode is repaired. An entry naming the installed body means the bundle is its successor and is written, whatever `version:` says (live: telegram bundled v195 over a live body at v603). *(3) The entry names a different body.* The bundle is not that body's successor, so the installed body is kept and the conflict is reported. The report is one line per cap with both hashes and with the hash to record, because the bundle alone cannot end it: reconcile `caps/bundled-caps/<name>/CAP.md` with the installed body, then record that body's digest as `derived_from_hash`.

Both daemon paths answer to the same provenance. The import (`handleSaveCap`, `internal/daemon/handler_caps.go`) refuses a file whose version is below `cap_imported_version` and a file at exactly that revision while the row has been changed since that import, with two exceptions: a body the installation's manifest names was put there by the installer on purpose, so it is imported and the row adopts its revision; and a row that carries a repair on top of the body it imported is the later state, so a delivery below its watermark is refused and reported instead of overwriting the edit. A delivery ABOVE the watermark takes the file, repaired row or not: an installation that was auto-corrected still has to receive a newer bundle. The export (`WriteCapToDisk`, `internal/daemon/cap_sync.go`) writes the row over a file whose version is below the row's, except when the file is a body the manifest names and the row is clean: either the row's own source is a later revision than the file (a deliberate step back, logged with both numbers) or the row was imported from exactly this file and has nothing of its own to add. Rewriting a clean file is what must not happen, because a rewritten file no longer hashes to its manifest entry and the next release would only be reported as a conflict. Where the manifest names something else, both paths keep the counters' decision and log the pair (`the file hashes to X, the installed manifest names Y`), so a hand-edited body and a lost install stay distinguishable. Versions are per-cap revision counters, not release numbers: `save_cap` raises them, and the repo copy stays where it was. Measured 2026-10-23, under the revision gate that is gone: telegram bundled v195 against live v603, cap_search v97 against 549, adapter_e2e_test v69 against 518, changelog_check v63 against 517, cap_collect v97 against 551, chrome_headless v2 against 3. All six were silently inert. Under provenance they surface on the next `yesmem migrate` as conflicts, because the shipped manifest is self-hashed and claims nothing about a live body.

Order of a rollout: `make deploy` first (rebuilds the binary that carries the new bundled source), then `yesmem migrate` (runs `InstallBundledCaps`). Reversed, the old binary's embedded copy is what gets installed. A repo copy that goes back to an older body is a divergent bundle: it is reported, not installed. Shipping an older body means recording it as the derivation of the body it replaces.

### Storage

`caps.db` holds:

- `caps` — name, version, body, scope (`global` | `project:<name>`), updated_at
- `cap_<name>__<table>` — per-cap user-defined tables created on demand via `cap_store(action="create_table"|"upsert"|"query"|"delete"|"list_tables")`
- `learnings` rows tagged `category="cap_proposed"` carry the auto-correct queue; `cap_proposal_decide` accepts or rejects each proposal

Full schema in `yesdocs/architecture/db-schema.md`.

### Bash Polyfills for Caps

Cap scripts declared with `runtime: bash` execute in the daemon's sandboxed environment — they don't have access to the Claude Code REPL or MCP tools. YesMem provides two polyfill functions that bridge this gap via Unix socket RPC to the daemon:

**`store()` — Key-Value Persistence**

```bash
store '{"capability":"proxy_health","action":"upsert","table":"reports","data":"{...}"}'
```

Takes **one positional argument: a JSON document**, forwarded verbatim to `yesmem store` (which writes to `caps.db` via the `cap_store` RPC). There are no key/value flags — the shape is the same as the REPL `store({...})` call, just serialized by hand. Used by cap handlers that need to persist state between invocations (e.g. Telegram last-read-message cursor, RSS last-fetch timestamp).

**`llm()` — LLM Completion**

```bash
result=$(llm "haiku" "You are a summarizer" "summarize this text: $long_text" "" "")
```

Takes **five positional arguments, in this order**: `model` (`haiku`, `sonnet`, `opus` or a full ID; empty = configured default), `system`, `prompt`, `session` (empty = new session), `tools` (the literal `tools` enables tool calling). The shell function builds a JSON body with `yesmem json` and calls `yesmem llm-complete`. There are no `--model` or `--max-tokens` flags, and no output-token limit is passed.

Both polyfills are defined in `internal/daemon/handler_cap_exec.go` and injected into the sandboxed shell before the handler body (after `set -e`). The socket and the `yesmem` CLI they need are registered as read-only mounts, and a missing mount refuses the run instead of failing later with an unnamed symptom.

Cap-Spec v1.1 lives in the sibling project `~/projects/cap-spec`. The `yesmem-cap-builder` bundled skill (336-line `SKILL.md` + recipes, api-reference, gotchas side-files) is the canonical reference for landing a working cap in one pass — it covers the REPL VM allowlist, the `sh` 30 KB wall, `sanitize_where`, schema rules, the bundled-cap DB write-back lifecycle (including pre-commit version sync), and the jq label/apostrophe quirks.

---

## 6. Secret Sanitization

`SecretRedactor` (`internal/sanitize`) is a single-pass regex-based redactor that replaces matches with `[REDACTED:<kind>]` markers. With `secrets_sanitization.enabled` switched on it runs at every persistence boundary so a leaked credential never reaches durable storage — neither in summaries, scheduled-job logs, briefings, nor extraction results. The switch defaults to `false`: without it no path wraps its clients for redaction, and persisted text keeps its secrets verbatim.

### Pattern Kinds (15)

| Kind | What it catches |
|------|------------------|
| `anthropic_api_key` | `sk-ant-…` keys |
| `openai_api_key` | OpenAI `sk-…` and project keys |
| `aws_access_key_id` / `aws_secret_access_key` | Long-lived AWS credentials |
| `github_pat` | Classic and fine-grained GitHub PATs |
| `jwt` | Three-segment Base64URL JWTs |
| `bearer_token` | `Authorization: Bearer …` and other Bearer-prefixed strings (broadened beyond the Authorization header in commit `0e0cb94`) |
| `password_in_url` | `proto://user:pass@host` URLs |
| `generic_api_key` | High-entropy `KEY=…` / `TOKEN=…` env-style strings; charset includes `./+=` for base64-shaped tokens (`2e819a2`) |
| `ssh_private_key_block` / `gpg_private_key_block` | Multi-line key blocks |
| `ipv4_public` | IPv4 addresses outside RFC1918 ranges |
| `hex_secret` | Long uppercase/lowercase hex strings |
| `email` / `phone` | Personal identifiers; phone regex tightened to reject IPv4-like dotted strings (`7d7e1a6`) |

### Decorator Chain

`SanitizingClient` wraps any `LLMClient`:

- **Sanitize-before** — outbound prompts pass through the redactor before hitting the upstream API.
- **Sanitize-after** — inbound responses pass through again so model-echo of unredacted prompts cannot recontaminate logs.
- **Inner-error path** — even when the wrapped client fails, the partial response is sanitized before the error bubbles up.

The decorator wraps the daemon's LLM clients where the redaction is part of the contract — extraction, briefing, summarize and the headless fallback among them. It is not a blanket "all six call sites" rule: wiring happens at assignment time so no unwrapped client can be reused, and a path that constructs its client directly bypasses it.

The scheduler also redacts `Command`, `ErrorMsg`, headless stdout, and stderr (`cf66345`) before persisting to `bash_job_runs`.

### Allowed Exceptions

`AllowedExceptions` in `config.yaml` lists strings that bypass redaction. Match is **full-match** (anchored both ends), not substring, so `MY_PUBLIC_TOKEN` does not unmask `MY_PUBLIC_TOKEN_FOR_TEST`. Redacted spans are replaced with `[REDACTED:<kind>]` (e.g. `[REDACTED:aws_secret_access_key]`). The decorator-order contract (sanitize wraps the client, never the other way around) is documented in `internal/extraction/sanitizing_client.go`; there is no `SANITIZING_CLIENT.md` in the repository.

### Configuration

```yaml
secrets_sanitization:
  enabled: true
  allowed_exceptions:
    - "TEST_FIXTURE_KEY"
    - "EXAMPLE_BEARER_PUBLIC_DEMO"
```

When `enabled: false` (the config default), the wrapper is a no-op pass-through; production deployments should enable it.

---

## 7. Sandbox Execution

Bash-mode scheduled jobs and headless agent invocations that opt in run inside `ai-jail`, a network-restricted, filesystem-restricted Linux sandbox. The daemon resolves the sandbox profile per fire and wraps the command via `BuildSandboxedCommand` (bash) / `WrapExecArgs` (headless) before invoking `exec.Cmd`.

### Profiles

| Profile | Network | Filesystem | Use case |
|---------|---------|------------|----------|
| `none` | Unrestricted | Unrestricted | Dev / trusted local caps. **Rejected on `scope: project` caps** — project caps must declare a non-`none` sandbox to prevent untrusted cap code from wiping the worktree (`b5481cf`) |
| `standard` | Allowed ports configurable (default `80,443`) | Read-only `/`, writable `$CWD` | Default for scheduled bash jobs |
| `strict` | None | Read-only `/`, writable scratch only | Untrusted research caps, web-scraping bundles, third-party adapter caps |

Profiles are persisted in `scheduled_jobs.sandbox` (job-level) and optionally overridden per script via `Script.Sandbox` in `CAP.md` (script-level). Resolution: explicit script value > job value > daemon default. When the script-level value matches the job-level value, the override log line is suppressed (`e8bb0ff`) to keep heartbeat output quiet.

### ai-jail Integration

The daemon:

1. Resolves the `ai-jail` binary path on startup (downloaded from a pinned GitHub release, extracted from the tarball).
2. Wraps `exec.Cmd` via `BuildSandboxedCommand` for bash-mode jobs and `WrapExecArgs` for `executeHeadless`.
3. Fails closed when the sandbox is unavailable — auto-correct will not silently re-fire an unsandboxed command on systems where the binary is missing.

`.ai-jail` sandbox configs are git-ignored so per-machine tweaks do not leak into commits.

### Caveats

- **In-memory pointer.** `fireJobBash` reads `job.Sandbox` from `s.jobs[id]`, not per-fire from the DB. A direct SQL `UPDATE scheduled_jobs SET sandbox=...` does not take effect until daemon restart or until the job is re-created via `mcp__yesmem__schedule(action="delete"|"create")`.
- **save-cap race for script-level sandbox.** When two sessions edit the same cap in parallel (e.g. `telegram` in two worktrees), `save_cap` writes overwrite each other every few minutes. Per-script `sandbox=none` declared via `save_cap` is therefore unstable across parallel sessions; use the job-level `scheduled_jobs.sandbox` (decoupled from `save_cap`) for stable overrides.
- **Auto-correct + sandbox.** When the sandbox profile is unavailable, auto-correct refuses to apply a fix; this is intentional fail-closed, not a bug.

---

## Differentiators

What makes YesMem fundamentally different — not "also does X", but "nobody else can do X":

### The Moat: Wire-Level Control

1. **Invisible Memory** — YesMem sits between Claude Code and the Anthropic API as an HTTP proxy. Memory is injected, context is compressed, and sessions are managed *before the model sees anything*. The user never calls a "remember" tool — it just happens. No other memory system operates at the wire level.
2. **Infinite Thread** — Conversations don't hit context limits. The proxy progressively compresses old messages through 4 decay stages (full → summary → stub → collapsed), with sawtooth cache management that recovers context on demand. Other systems tell you to start a new chat.
3. **Real-Time Background Extraction** — A forked agent pipeline extracts learnings *while the session is still running*, not after it ends. By the time you finish a debugging session, the insights are already indexed and searchable.
4. **Associative Priming** — Every user message triggers automatic semantic search. Relevant memories are injected into the next model response — invisibly, at the proxy layer. The model gets context it didn't ask for, and the user gets answers informed by past sessions without doing anything.

### Things Nobody Else Has

5. **Source Attribution** — Every learning tracks its origin: `user_stated > agreed_upon > claude_suggested > llm_extracted`. We analyzed 31 memory-related research papers. None of them track provenance. This means YesMem can distinguish "the user told me this" from "I inferred this" — and weight them differently.
6. **Fixation Detection** — When the model gets stuck in a loop (same error, same fix attempt, same failure), YesMem detects it automatically and penalizes the scoring of the learnings that caused the loop. Other systems let you spiral; YesMem breaks the cycle.
7. **Gotcha Prevention** — The hook system intercepts tool calls *before execution*. Failed commands are auto-learned; on the next attempt, relevant gotchas are injected as warnings so the model can course-correct. After 5 repeated failures of the same pattern (and a match score of at least 4), the command is hard-blocked — the model can't make the same mistake a sixth time. Prevention escalates: context first, then a wall.
8. **Multi-Agent Memory Safety** — Multiple Claude Code agents share long-term memory with conflict detection, dialog-extract-block protocol, agent-role scoping, and auto-broadcast. This isn't "shared database" — it's coordinated memory with write isolation.
9. **Emotional Memory** — Sessions are rated by intensity (breakthroughs, frustration, pivots). Learnings from high-intensity sessions score higher in retrieval. The system remembers *what mattered*, not just what happened.

### Architectural Advantages

10. **Zero-Dependency Binary** — Single Go binary, about 143 MiB (the embedded SSE weight parts account for roughly 103 MiB of that), with the 512-dimensional embedding model compiled in via `go:embed`. No Python, no pip, no Docker, no external services. `curl | sh` and you're running.
11. **Two-Layer Architecture** — Raw material (full session transcripts, indexed documents) is preserved for deep search. Distilled learnings provide fast access. You get both precision and recall — grep through old conversations *or* get instant structured answers.
12. **Trust-Based Evolution** — Learnings have trust levels. A `user_stated` fact resists being overwritten by an `llm_extracted` inference. High-trust knowledge requires high-trust evidence to supersede. This prevents hallucination-driven memory corruption.
13. **Ebbinghaus Decay** — Memory fading follows spaced-repetition curves grounded in cognitive science, not arbitrary TTLs. Frequently accessed learnings stay fresh; unused ones fade predictably. Decisions and pivot moments decay slower than routine observations.
14. **Orthogonal Scoring** — Five independent counters (use, citation, retrieval, injection, feedback) prevent a learning from becoming "important" just because it was retrieved often. A learning that gets injected 50 times but never cited is noise, not signal.
15. **Persona Continuity** — Not just fact storage — a persistent identity model that evolves across sessions. Traits, preferences, communication style, expertise areas. Session 1036 knows who you are because sessions 1–1035 built that understanding incrementally.

### Project Exclusion

- **`exclude_projects`:** Projects can be excluded from session indexing and extraction by adding them to `exclude_projects: []` in config.yaml. Applies to both the indexer and the opencode scanner. Useful for build directories, temporary projects, or noise-heavy repositories.


## 8. Scheduled Agents

Cron-based task scheduler built into the daemon. Define recurring or one-shot jobs that spawn agents automatically.

### Three Execution Modes

| Mode | How | Visibility | Overhead | Use case |
|------|-----|-----------|----------|----------|
| `agent` | Spawns PTY bridge + tmux window | Visible | Full briefing + agent lifecycle | Complex tasks, debugging, coding plans |
| `headless` | Runs `claude -p` as subprocess | Silent | Minimal — no lifecycle management | Routine automation, cron jobs, data collection |
| `bash` | Executes a cap's `runtime: bash` script directly in the daemon | Silent | Smallest — no LLM call, no agent process | High-frequency polling (Telegram, RSS, health probes), deterministic shell pipelines, anything where a single shell script is the right primitive |

### MCP Interface

Single `schedule` tool with four actions:

```
schedule(action="create", name="daily-reddit", cron="0 9 * * *", prompt="...", mode="headless")
schedule(action="list")
schedule(action="delete", id="sched-daily-reddit-12345")
schedule(action="run", id="sched-daily-reddit-12345")   # manual trigger
```

### Task Delivery

The scheduler writes the task prompt to a job-specific scratchpad section **before** spawning the agent. The agent reads its task from the briefing — no relay timing issues.

```
Scratchpad section: sched-<job-name>
Content: ## SCHEDULED TASK [<job-name>]
         Job-ID: <id>
         <wrapped prompt with focus instructions>
```

### Agent Lifecycle

- **Pre-spawn cleanup** — stops any existing agent on the same section
- **Idle timeout** — 10-minute unified timeout for running, paused and idle agent states
- **Watchdog goroutine** — the scheduler's own poll loop runs every 15 seconds and stops idle agents

### Headless Mode

Uses `claude -p` (Claude Code non-interactive mode) as a daemon subprocess:
- Full MCP tool access (caps, cap_store, haiku, scratchpad)
- Runs through the proxy (subscription-based, no API key needed)
- Output captured and written to scratchpad
- No tmux window, no PTY bridge, no watchdog needed

### Caps as Automation Primitives

Caps are ideal for scheduled tasks because they are predictable: defined schema, known handler, deterministic behavior. The agent activates the cap and executes it — no improvisation needed.

### Bash Mode + Auto-Correct

Bash-mode jobs execute a single named script from a capability's `CAP.md` (`runtime: bash`). The scheduler resolves the script via `findBashScript(meta, job.ScriptName)` — explicit `script_name` is mandatory whenever a cap declares multiple bash scripts, otherwise the first declared script wins silently and the job appears to "succeed" while running the wrong code.

A heartbeat-driven error handler (every 5th beat of the daemon's 1s ticker, so every 5 s) processes failed runs in the background:

1. **Diff classifier** — compares the cap's current body against a Sonnet-suggested fix and labels the diff as *trivial* (1-2 line typo, regex tweak) or *substantial* (logic change, new branch, removed assertion).
2. **Trivial path** — auto-applies the fix via `save_cap` and re-fires the job.
3. **Substantial path** — writes the proposal as a `learnings.cap_proposed` row; user reviews and accepts/rejects via the `cap_proposal_decide` MCP tool.
4. **Rate-limits** — T4 caps the per-cap auto-correct retry budget; T5 blocks runaway re-fires when the same script fails N times in a row.
5. **Sandbox-aware** — fails closed when the configured sandbox profile is unavailable instead of silently downgrading to unsandboxed execution.

**Cap consolidation pattern:** Poll-and-reply caps (e.g. Telegram) run both polling and replying in a single Bash script via a deadline-loop: `poll(), reply(), retry if more data, exit at deadline`. This avoids spawning separate headless agents per incoming message and keeps state closures within the same AI-jail process for the full fire interval.

Each bash run is persisted in `bash_job_runs` (CRUD via `internal/storage/bash_run.go`) with stdout/stderr; both streams are routed through `SanitizingClient` when secret redaction is enabled, so credentials accidentally echoed by a script never reach the durable log.

**Caveat — in-memory `job.Sandbox`:** the scheduler reads `job.Sandbox` from its in-memory pointer (`s.jobs[id]`), so a direct `UPDATE scheduled_jobs SET sandbox=...` does not take effect until daemon restart or until the job is re-created through `mcp__yesmem__schedule(action="delete"|"create")`.

### Cap Consolidation Pattern

When a capability needs both poll-based ingestion *and* reactive replies, run them in **one** scheduled bash script with an internal deadline-loop instead of two separate schedules:

```bash
DEADLINE=$(($(date +%s) + 55))
while [ $(date +%s) -lt $DEADLINE ]; do
  REMAIN=$((DEADLINE - $(date +%s)))
  [ $REMAIN -le 4 ] && break
  # 1. long-poll for new updates (timeout bounded by REMAIN)
  # 2. store any updates locally
  # 3. only attempt expensive reply (e.g. claude -p) if REMAIN > 30
done
```

Why this matters: a separate "reply" job with `interval_seconds: 1` (or empty cron) becomes a daemon-tick spinner. The `telegram` cap was firing 60+ bash forks per minute against an idle reply queue, even though only one fire per minute (the cron-driven poll) ever touched the network. Consolidating into a single 55-second long-fire amortizes the fork cost and keeps user-facing reply latency under one cron tick. The `REMAIN > 30` guard prevents starting a new `claude -p` reply that would extend past the next fire boundary.

Verified on the `telegram` cap (poll + reply merged into the `telegram_poll` script, 2026-05-04). Reply round-trip is around 41 seconds; the `REMAIN > 30` cutoff leaves enough headroom for the next fire to start cleanly without overlap.

### Comparison with Anthropic Scheduled Tasks

| | Anthropic Cloud Routines | Desktop Scheduled Tasks | YesMem Scheduler |
|---|---|---|---|
| Runs on | Anthropic cloud | Local (app open) | Local (daemon) |
| Memory | None (fresh each run) | None | Full persistent memory |
| Local files | No (fresh clone) | Yes | Yes |
| MCP servers | Connectors only | Local | Full local MCP |
| Caps/Tools | N/A | N/A | Reusable caps + cap_store |
| Cost | API tokens + $0.08/h | Subscription | Subscription |
| Limits | Pro: 5/day, Max: 15/day | Desktop-bound | Unlimited (self-hosted) |

The differentiator list lives once, in the "Differentiators" section above (§ between Sandbox Execution and Scheduled Agents). It is not repeated here.

## Internal Packages (39)

| Package | Purpose |
|---------|---------|
| `archive` | Permanent JSONL archiving (protects against Claude Code's 30-day cleanup) |
| `benchmark` | LoCoMo benchmark evaluation framework |
| `bloom` | Per-session bloom filters (~4KB each) for fast negative lookups |
| `briefing` | Session briefing generation, milestones, dedup, i18n, formatting |
| `buildinfo` | Build-time version injection |
| `capfile` | Cap-Spec v1.1 parser/serializer for `CAP.md` files: scripts (tool/handler), runtimes (repl/bash), per-script sandbox metadata, `DatabaseSQL` hydration via DDL extraction |
| `capblob` | Chunked blob store for cap payloads above the REPL output wall (`DefaultChunkSize` 20 KB) |
| `channeltext` | Neutralization helpers for text that is injected into a channel block (one-line, marker-safe) |
| `claudemd` | Operative reference generation from learnings |
| `codescan` | Code intelligence: CBM scanner, scan cache keys (repo-root based), Code Map rendering, `project_scan` persistence |
| `clustering` | Agglomerative clustering on learning embeddings (cosine 0.85) |
| `config` | YAML configuration loader |
| `daemon` | RPC handlers (~70 methods), lifecycle, Unix socket server, extraction orchestration, scheduler with three execution modes (agent/headless/bash) and heartbeat-driven auto-correct loop |
| `embedding` | Vector DB, SSE inference (512d, go:embed, pure Go) |
| `extraction` | Multi-phase LLM pipeline (extract → evolve → narrate), batch cycle every 2h, `SanitizingClient` decorator wraps LLM calls when secret redaction is enabled |
| `graph` | File/command/project association graph |
| `hints` | Timestamp hints with anti-habituation rotation (33 variants) |
| `hooks` | Claude Code hook handlers (briefing, check, failure, think, resolve, session-end, idle) |
| `httpapi` | HTTP API adapter for daemon (external integrations) |
| `indexer` | File watcher, session tracking, subagent detection, JSONL archiving |
| `ingest` | Doc ingestion, format-aware chunkers (Markdown via goldmark, RST via custom parser), rich metadata extraction, hash tracking, skill detection |
| `ivf` | Inverted file index for vector search |
| `logrotate` | Size-based rotation for the daemon log (50 MB, 5 backups) |
| `mcp` | MCP server (stdio), daemon proxy client, tool registration (73 tools, including `cap_proposal_decide`, `schedule`, `cap_store`) |
| `models` | Shared data types, scoring (turn-based decay, contextual, origin-aware multiplier), model ID resolution |
| `opencode` | Reader for opencode's own config surfaces (`opencode.json`, models.json, auth.json) |
| `orchestrator` | Multi-agent lifecycle: PTY bridge, spawn, heartbeat, crash recovery |
| `parser` | JSONL session parser (Claude Code + Codex) |
| `repair` | Attribution repair for learnings filed under the wrong session |
| `repo` | Repository root resolution (worktree → main repo, submodule boundaries) |
| `proxy` | Infinite-thread HTTP proxy (stubs, decay, narrative, collapse, reexpand, sawtooth, signals, reminders, associative context, compression, fixation detection, skill injection); FREEZE/RESTORE symmetry with eager-stub memory layer; prompt-flow isolation between `claude` and `codex` client paths |
| `sanitize` | `Sanitizer` interface and `SecretRedactor` with 15 pattern kinds (Anthropic/OpenAI/AWS/GitHub/JWT/Bearer/SSH/GPG/IPv4-public/hex/email/phone/...); `AllowedExceptions` whitelist with full-match semantics |
| `setup` | Interactive configuration wizard, bundled-caps installer (sha256 compare, an installed body with a higher revision is kept; writes to `~/.claude/caps/`) |
| `storage` | SQLite persistence (~37 tables across 4 DB files: `yesmem.db`, `messages.db`, `runtime.db`, `caps.db`), migrations, FTS5, CRUD, read-only `OpenReadOnly` for CLI subcommands; full schema reference in `yesdocs/architecture/db-schema.md` |
| `terminals` | Terminal emulator detection and launch helpers for agent windows |
| `textutil` | Token estimation, text similarity, bigram-Jaccard, one-line log sanitising |
| `update` | Auto-update: semver parsing, GitHub Release checker, download + checksum, atomic swap, orchestrator |
| `wikidir` | Wiki output directory ownership: marker files and safe target resolution |
| `wikirender` | Renders learnings, sessions, files and code intelligence into the Markdown wiki |

---


## Deployment

```bash
cd ~/memory/yesmem
make build                      # Build with correct GOROOT/GOPATH/GOCACHE
make deploy                     # Build + deploy to ~/.local/bin/yesmem
```

After deploy: `/mcp reconnect` in Claude Code to restart MCP server.

### CI/CD Pipeline

GitHub Actions workflows (`.github/workflows/`):
- **ci.yml** — Runs on every push to `main` + PRs. Three jobs: `test` (build and tests on an Ubuntu + macOS matrix), `plugin` (Bun suite of `plugins/opencode-yesmem-v2`, with `bun install --frozen-lockfile` first), and `race` (`go test -race` with an explicit `-timeout`). The workflow token is read-only (`permissions: contents: read`).
- **release.yml** — Runs on version tags (`v*`). Tests on Ubuntu + macOS must pass, then GoReleaser builds binaries for `linux/darwin × amd64/arm64`. Creates GitHub Release with checksums.
- Release flow: `git tag v1.0.3 && git push origin v1.0.3` → GitHub Actions builds + publishes.

**Race gate.** `-race` runs in the `race` job and in `make test-race`, and nowhere else. It is deliberately not a whole-package run: on this host with a warm cache `go test -race ./internal/hooks` takes 653s and `./internal/storage` 875s, and the whole daemon suite exceeds go test's 600s default and panics in `TestLos57eClaudeResumeCarriesTheAllowlist`. The gate therefore runs `internal/proxy` whole and, for `internal/storage`, `internal/daemon` and `internal/hooks`, only the tests that exercise the concurrent wiring, selected by `RACE_RUN` in the Makefile (24 tests: 10 storage, 9 daemon, 5 hooks, about 50s in total instead of roughly 26 minutes). `RACE_RUN` is the single source for that selection: the Makefile target uses it directly and the CI job repeats it verbatim. The package list stands in three places (`RACE_PKGS`, the Makefile recipe, the CI job) and is cross-checked in both directions, so a package present on one side only turns the guard red instead of silently changing the gate. The guard also rejects a run without a raised timeout, a commented-out command, a swallowed exit status, a disabled job, and any name in the selection that no longer exists as a test function (`go test -run Ghost` exits 0 with "no tests to run"). The boundary stays honest rather than complete: a race in a test outside the selection is not caught here, `release.yml` runs neither the plugin suite nor `-race`, so a tag push is gated on neither, and nothing requires the `race` job in the repository's branch protection.

**Test surface.** `make test` runs the Go suite (`test-go`) and the plugin's bun suite (`test-plugin`); `make test-go` and `make test-plugin` run either half alone. The plugin suite is not self-contained: `code_nav_test.ts` reads the shared fixture `internal/hooks/testdata/codenav_parity.json`, so it only runs inside the repository, and `bun` has to be found through `BUN` (PATH lookup with a `~/.bun/bin/bun` fallback) because it is not on PATH in every shell. `make update` validates through `make test`, so it now needs bun too; without it the target stops after the dependency update with the message from `test-plugin`. CI never runs `make test` itself, so the recipes are covered by the guard test's structure scan and by local runs, not by a workflow.

**systemd:** User services with `After=network-online.target` + `Wants=network-online.target` (required for DNS after suspend/resume).

**Optimal migration/rebuild sequence:**
```bash
yesmem reextract                # re-extract all sessions
yesmem embed-learnings --all    # embed all learnings
yesmem regenerate-narratives    # regenerate narratives
yesmem bootstrap-persona --all  # re-extract persona
yesmem synthesize-persona       # re-synthesize directive
yesmem claudemd --all           # regenerate ops files
```

---

