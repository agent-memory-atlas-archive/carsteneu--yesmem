# Config Reference (`config.yaml`)

Complete reference for all settings in `~/.claude/yesmem/config.yaml`.
Missing keys are filled with defaults from `internal/config/config.go:Default()`.

---

## extraction — Extraction Pipeline

Controls LLM-based knowledge extraction from past sessions.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `model` | string | `sonnet` | Pass 2 — extraction model. Shortnames: `haiku`, `sonnet`, `opus`, or full model ID. |
| `summarize_model` | string | `haiku` | Pass 1 — summarization (compresses session chunks). Haiku is sufficient. |
| `quality_model` | string | `sonnet` | Pass 2 — quality refinement (dedup, relevance, contradictions). Falls back to `narrative_model`. |
| `narrative_model` | string | `opus` | Narrative generation (session handovers, project profiles, persona). |
| `mode` | string | `prefiltered` | `full` = send everything / `prefiltered` = rule-based pre-filter. |
| `chunk_size` | int | `25000` | Sessions larger than N tokens are split into chunks. |
| `auto_extract` | bool | `true` | Start extraction automatically after session end. |
| `max_age_days` | int | `0` | Only extract sessions from the last N days. `0` = all. |
| `max_per_run` | int | `30` | Max sessions per daemon run. `0` = unlimited. |
| `min_session_age_hours` | int | `24` | Skip sessions younger than N hours (waits for replication). |

---

## llm — LLM Backend & Budgets

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `provider` | string | `auto` | `auto` (API if key present, else CLI), `api` (HTTP API), `openai`, `openai_compatible`, `cli` (Claude CLI binary). |
| `claude_binary` | string | `""` | Path to Claude CLI binary (only needed for `provider: cli` if not in PATH). |
| `daily_budget_extract_usd` | float64 | `5.0` | Daily budget for extraction + evolution (USD). `0` = no limit. API providers only. |
| `daily_budget_quality_usd` | float64 | `2.0` | Daily budget for narratives + persona (USD). `0` = no limit. API providers only. |
| `max_budget_per_call_usd` | float64 | `1.0` | Max budget per individual call (CLI: `--max-budget-usd`). `0` = no limit. |

---

## evolution — Knowledge Evolution

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `auto_resolve` | bool | `true` | Automatically detect and resolve contradictions between learnings (older learning marked as superseded). |
| `unfinished_ttl_days` | int | `30` | Open tasks/TODOs stay active for N days, then archived. `0` = active forever. |

---

## briefing — Session Start Briefing

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `detailed_sessions` | int | `3` | Number of recent sessions shown with full details (older appear as one-liners). |
| `other_projects_days` | int | `90` | Time window for "Other Projects" section (days). |
| `max_tokens` | int | `5000` | Max briefing size in tokens (safety limit). |
| `dedup_threshold` | float64 | `0.4` | Similarity threshold for dedup (0.0-1.0). Lower = more aggressive. |
| `max_per_category` | int | `5` | Max learnings per category in the briefing. Rest available via `get_learnings()`. |
| `languages` | []string | `["de","en"]` | ISO 639-1 codes for stop-word filtering during dedup. |
| `remind_open_work` | bool | `true` | Inject instruction to mention open work at session start. |
| `user_profile` | bool | `true` | Include synthesized user profile in briefing. |

---

## proxy — Infinite-Thread Proxy

Sits between Claude Code and the Anthropic/OpenAI API, compressing old messages.

### Core Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Start proxy with daemon. |
| `listen` | string | `:9099` | Listen address/port. Claude Code routes via `ANTHROPIC_BASE_URL=http://localhost:9099`. |
| `target` | string | `https://api.anthropic.com` | Upstream API (Anthropic). |
| `openai_target` | string | `https://api.openai.com` | Upstream API for OpenAI-format clients. |

### Compression

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `token_threshold` | int | `250000` | Trigger stubbing when conversation exceeds this token count. |
| `token_minimum_threshold` | int | `100000` | Compress down to this token count. |
| `keep_recent` | int | `10` | Last N messages stay uncompressed (working context). |
| `sawtooth_enabled` | bool | `true` | Sawtooth cache optimization (frozen prefix between stub cycles). |
| `cache_ttl` | string | `ephemeral` | Cache TTL for `cache_control` blocks: `ephemeral` (5 min) or `1h` (2× write cost). |
| `usage_deflation_factor` | float64 | `0.7` | Scale input tokens reported to Claude Code (suppresses "Context low" warning). `0` = off. |
| `token_thresholds` | map | (see below) | Model-specific thresholds. |

**Default `token_thresholds`:**
```yaml
token_thresholds:
  opus: 180000
  sonnet: 180000
  haiku: 130000
  gpt-5: 180000
  codex: 180000
  deepseek: 600000
  glm-5.2: 500000
```

### Cache Keepalive

Prevents cache expiry during idle periods via periodic pings.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `cache_keepalive_enabled` | bool | `true` | Send keepalive pings. |
| `cache_keepalive_mode` | string | `5m` | `auto` (detect from response), `5m`, `1h`. |
| `cache_keepalive_min_messages` | int | `10` | Do not ping a thread that has fewer messages than this. |
| `cache_keepalive_pings_5m` | int | `5` | Pings per idle phase when TTL=5min. |
| `cache_keepalive_pings_1h` | int | `1` | Pings per idle phase when TTL=1h. |

### Code Navigation

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `code_nav_mode` | string | `block` | `block` (block shell/read navigation on indexed files, suggest code-nav tools), `nudge` (hint only), `off`. In `nudge` the call runs and the advice is prepended to its tool result; in `off` nothing is checked. Read case-insensitively, surrounding spaces are trimmed; an unknown value blocks. Both layers read it: the Claude Code PreToolUse hook and the opencode plugin's `code_nav` guard, which re-reads config.yaml at most once a minute. The 2-strike auto-allow and `dismiss_code_nav` apply in `block` only. |

One `dismiss_code_nav` call lifts the block for the rest of the session; the threshold is not configurable. `code_nav_dismiss_count` is retired: the field stays in the struct only so an existing config.yaml is not reported as carrying an unknown key, and nothing reads it.

### Prompt Injections

Each injection can be toggled individually. Default: all `true` except as noted.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `prompt_ungate` | bool | `true` | `[deprecated]` Use `claude_prompt.prompt_ungate`. Strip CLAUDE.md subordination disclaimer. |
| `prompt_rewrite` | bool | `false` | `[deprecated]` Use `claude_prompt.prompt_rewrite`. Strip output-throttling + inject quality directives. |
| `prompt_enhance` | bool | `false` | `[deprecated]` Use `claude_prompt.prompt_enhance`. CLAUDE.md authority boost + comment discipline + persona tone. |
| `prompt_tool_prefs` | bool | `true` | `[yesmem-tool-prefs]` Edit/Write preference + error-semantics warning. |
| `prompt_output_discipline` | bool | `true` | `[yesmem-output-discipline]` No preamble + no skill eval + exploratory heuristic. |
| `prompt_coding_discipline` | bool | `true` | `[yesmem-coding-discipline]` Read-before-propose + no brute force + no half-finished. |
| `prompt_beweislast` | bool | `true` | `[yesmem-beweislast]` Fabrication guard + claim vs proof + tool-result honesty. |
| `prompt_scope_discipline` | bool | `true` | `[yesmem-scope-discipline]` Deliver A not A+B+C + adjacent findings separate. |
| `prompt_delegation_contract` | bool | `true` | `[yesmem-delegation-contract]` Self-contained prompts + parallel dispatch. |
| `prompt_clarify_first` | bool | `true` | `[yesmem-clarify-first]` Only clarify when alternative interpretations produce materially different work. |
| `prompt_code_tools_first` | bool | `true` | `[yesmem-code-tools-first]` Prefer MCP code-navigation tools over agent spawns. |
| `prompt_wiki_first` | bool | `true` | `[yesmem-wiki-first]` Check the project wiki before broad exploration. |
| `prompt_fable` | bool | `true` | `[yesmem-fable]` Baseline/rollback/readback discipline for high-stakes changes. |
| `prompt_pattern_suggest` | bool | `true` | Record repeated shell-command shapes for cap-suggestion analysis. |
| `effort_floor` | string | `""` | Minimum effort level: `low`, `medium`, `high`, `max`. Empty = off. |
| `skill_eval_inject` | string | `silent` | `true` (verbose eval output), `silent` (internal eval only), `false` (disabled). |

`complete_provider` is **not** a `proxy:` key: it lives in the `llm:` section (`llm.complete_provider`) and overrides the provider for the `llm_complete` RPC (e.g. `opencode`); empty means the configured `llm.provider`.

### Profile-Aware Prompt Configuration

Replaces the deprecated flat prompt flags. Each pipeline gets its own prompt profile; empty fields inherit from `shared_prompt`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `shared_prompt` | PromptProfile | `{}` | Base prompt profile for ALL pipelines. |
| `claude_prompt` | PromptProfile | `{}` | Claude Code override. Empty = inherit from shared_prompt. |
| `codex_prompt` | PromptProfile | `{}` | Codex override. Empty = inherit from shared_prompt. |
| `opencode_prompt` | PromptProfile | `{}` | OpenCode override. Empty = inherit from shared_prompt. |

A key you write explicitly always wins, including an explicit `false`: `no`, `off` and `false` all count as a statement, so `beweislast: no` under `opencode_prompt` switches off an injection inherited from `shared_prompt`. Only keys you leave out inherit.

### Provider Auto-Discovery

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `auto_configure_providers` | bool | `true` | Read models.json + opencode.json + auth.json at proxy startup to auto-configure provider routing. Set to `false` to disable. |
| `provider_targets` | map[string]string | `{deepseek: "https://api.deepseek.com"}` | Per-provider target URLs for OpenAI-compatible providers. Keys are matched as case-insensitive prefixes against model names. |

### Custom System Prompt

Replaces the default system prompt with the SYSTEM.md template for supported pipelines.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `custom_system_prompt.enabled_opencode` | bool | `true` | Inject SYSTEM.md for OpenCode/DeepSeek/OpenAI-compatible pipelines. |
| `custom_system_prompt.enabled_claude_code` | bool | `false` | Inject SYSTEM.md for the Claude Code pipeline. |
| `custom_system_prompt.enabled_codex` | bool | `false` | Inject SYSTEM.md for the Codex pipeline. |
| `custom_system_prompt.template_path` | string | `~/.claude/yesmem/SYSTEM.md` | Path to the system prompt template file. |

### Per-Model Feature Gates

Control which yesmem behavioral features are active per model/provider. Keys under `model_features` are model name prefixes matched case-insensitively (longest wins). A key a matching block does not name falls back to the same key under `feature_defaults`; a key the block does name wins, including a written `false`. Models not listed use `feature_defaults` for every key.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `model_features.<model>.skill_eval` | bool | `true` | Inject [skill-eval] instruction block. |
| `model_features.<model>.briefing` | bool | `true` | Inject yesmem briefing at session start. |
| `model_features.<model>.rules_reminder` | bool | `true` | Periodic reminder of project rules/guidelines. |
| `model_features.<model>.plan_checkpoint` | bool | `true` | Inject plan checkpoint reminders during long sessions. |
| `model_features.<model>.think_reminder` | bool | `true` | Inject hybrid_search() hint to check memory before assuming. |
| `model_features.<model>.think_reminder_min_chars` | int | `0` | Min user text length to trigger think_reminder. `0` = inherit from `feature_defaults` (there `0` = always). Set to `10` to skip short messages like "hi" or "ok". |
| `model_features.<model>.timestamps` | bool | `true` | Inject [HH:MM:SS] [msg:N] [+Δ] markers for temporal awareness. |
| `feature_defaults.<key>` | — | — | Same keys as above. Speaks for every key a matching model block does not name, and for every key of an unlisted model. |

Example: per-model gates for DeepSeek with think_reminder gated at 10+ chars:

```yaml
proxy:
  model_features:
    deepseek:
      think_reminder: true
      think_reminder_min_chars: 10
```

---

## embedding — Semantic Search

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `provider` | string | `sse` | `sse` (Stable Static Embeddings, 512d, embedded in the binary), `static` or `none`. There is no `local`/`api` provider and no `local.*` or `vector_db.*` section. |
| `search.method` | string | `""` | Search method (empty = auto: brute force, or IVF above the threshold). |
| `search.ivf_threshold` | int | `50000` | Switch to the IVF index above N vectors. |
| `search.ivf.k` | int | `0` | IVF cluster count (`0` = auto sqrt). |
| `search.ivf.nprobe` | int | `15` | IVF cluster search breadth. |

The vector store path is fixed at `~/.claude/yesmem/vectors/`; it is not configurable. Dimensions are fixed at 512 for the `sse` provider (the model is embedded, not downloaded).

---

## api — API Keys

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `api_key` | string | `$ANTHROPIC_API_KEY` | Anthropic API key. Lookup order: 1. env, 2. config, 3. `~/.claude/config.json`. |
| `openai_api_key` | string | `$OPENAI_API_KEY` | OpenAI API key. |
| `openai_base_url` | string | `$OPENAI_BASE_URL` | Custom OpenAI-compatible base URL. |

---

## signals — Cognitive Signals

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Gate for the inline reflection. When off, no signals are scanned at all. |
| `mode` | string | `reflection` | Inert — configured the removed async reflection call; kept so old files parse. |
| `model` | string | `haiku` | Inert — same reason. |
| `every_n_turns` | int | `1` | Inert — same reason. |

---

## claudemd — Operative Reference (`yesmem-ops.md`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Auto-generate `yesmem-ops.md` per project. |
| `max_per_category` | map | (see below) | Max learnings per category in the reference. |
| `refresh_interval` | string | `2h` | Regeneration interval (Go duration: `2h`, `30m`, `24h`). |
| `min_sessions` | int | `3` | Minimum sessions before generating a reference. |
| `output_file` | string | `yesmem-ops.md` | Output file (placed in project's `.claude/` directory). |
| `model` | string | `""` | Model (empty = `narrative_model`). |

**Default `max_per_category`:**
```yaml
max_per_category:
  gotcha: 15
  pattern: 10
  decision: 10
  explicit_teaching: 5
  pivot_moment: 5
```

---

## http — HTTP API Server

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Start HTTP listener. |
| `listen` | string | `127.0.0.1:9377` | Listen address. |
| `auth_token` | string | `""` | Bearer token (empty = auto-generated). |

---

## diagnostics — Diagnostics Endpoints

Off by default. The daemon reads config once at startup, so a change needs a daemon restart.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `pprof` | bool | `false` | Start the Go pprof endpoint. `YESMEM_PPROF` overrides it (`1`/`true`/`yes`/`on` forces on, `0`/`false`/`no`/`off` forces off). |
| `pprof_listen` | string | `127.0.0.1:7071` | Listen address. Must be loopback — a non-loopback host (`0.0.0.0`, `::`, a LAN address) is rejected and the endpoint does not start. |

The endpoint serves Go runtime profiles only — allocation sites, goroutine stacks and CPU samples, never session transcripts. It is **unauthenticated**: any process on this machine can read a profile (`/debug/pprof/cmdline` returns the daemon's argv), and on a machine with other logged-in users they can read it too.

Reading is not the only capability. `/debug/pprof/profile` and `/debug/pprof/trace` keep the daemon busy for as long as the request asks, so an unsupervised local user can start a CPU profile or an execution trace and let it run. Leave `pprof` off unless you are diagnosing something — it is a debugging aid, not a service, and `pprof_listen` must stay loopback for that reason.

Requests are fenced in three ways:

- every request must carry a loopback `Host` header (`localhost`, `127.0.0.1` or `::1`) — non-loopback hosts are rejected, which also blocks DNS rebinding;
- requests a browser marks as coming from another origin (`Origin`, `Sec-Fetch-Site: cross-site` or `same-site`) are rejected (403), so a page cannot start a profile through an `<img>` tag or a `fetch`. A request carrying none of those labels is admitted: any process on this machine, and a fetch a browser does not attribute to a page (for example from an extension, which sends `Sec-Fetch-Site: none`). The endpoint trusts this machine — the labels only separate a page from an operator;
- methods other than GET and HEAD are rejected (405).

The duration of a profile request is capped at 120 seconds — `profile` and `trace`, and any named profile that takes a `seconds` parameter to look at a window (for example `heap?seconds=300` for the allocation rate). A longer value is silently reduced, with no response header to show it.

**`block` and `mutex` stay empty.** The daemon never enables the block or mutex profiler (`runtime.SetBlockProfileRate`, `runtime.SetMutexProfileFraction`), so those two profiles exist but report no samples.

```yaml
diagnostics:
  pprof: true
  pprof_listen: "127.0.0.1:7071"
```

Read a heap profile:

```bash
go tool pprof http://127.0.0.1:7071/debug/pprof/heap
```

**`inuse_space` vs `alloc_space`** — the distinction that separates retention from churn:

| Metric | High value means |
|--------|------------------|
| `inuse_space` | Live heap still referenced — retention or a leak. |
| `alloc_space` | Total allocated since start — churn/spikes. With a low `inuse_space` the memory was already released by the GC. |

Pass `?gc=1` to sample from live data — it triggers a real garbage collection (stop-the-world) inside the daemon, so use it when you need a clean `inuse_space` reading. Switch between `-inuse_space` and `-alloc_space` inside `go tool pprof`.

---

## update — Auto-Update

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `auto_update` | bool | `true` | Automatically check for new versions. Set to `false` on dev machines. |
| `check_interval` | string | `6h` | Check interval (Go duration). |
| `channel` | string | `stable` | Update channel: `stable`. |

---

## agents — Agent Spawning

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `terminal` | string | `""` | Preferred terminal: `ghostty`, `kitty`, `gnome-terminal`, `alacritty`, `wezterm`, `xterm`. Empty = auto-detect. |
| `viewer_terminal` | string | `""` | Terminal for displaying agent sessions. Falls back to `terminal`. |
| `default_backend` | string | `claude` | Backend for spawned agents: `claude` or `opencode`. |
| `max_runtime` | string | `""` | Max runtime per agent (Go duration: `30m`, `1h`). Empty = **48h** default. Read at daemon start; there is no hot reload. |
| `max_turns` | int | `0` | Max relay turns per agent. `0` = 30 default. |
| `max_depth` | int | `0` | Max spawn depth (agent → sub-agent). `0` = 3 default. |
| `permission_kick` | bool | `true` | Send a double Enter when an agent sits in a permission dialog. **This grants the pending permission**: the Enter confirms the dialog's default selection ("Allow once"). It is a no-op only on the TUI's empty input line; in an interactive child process the Enter hits that process's default. |
| `permission_kick_delay` | string | `3m` | Stream-inactivity before the first kick (empty = 3m). |
| `permission_kick_interval` | string | `5m` | Re-kick interval (empty = 5m). |
| `token_budget` | int | `0` | Max output tokens per agent. `0` = no limit (the default is no limit, not a number). Overridable per spawn. |
| `themes` | map | `{}` | Agent type → opencode TUI theme for spawned agents (e.g. `yesloop: yesmem-loop`). Empty value disables the type's theme. See [agents.themes](#agentsthemes--agent-tui-themes). |

### agents.themes — Agent TUI Themes

Every spawned opencode agent gets a theme that names its type, so parallel agent
windows are distinguishable at a glance. The type is the section prefix:
`yesloop-agent-themes` → `yesloop`, `yesresearch-fusion` → `yesresearch`,
`subresearch-materials` → `subresearch`, `orchestrator` → `orchestrator`.

| Type | Theme | Accent |
|------|-------|--------|
| `yesloop` | `yesmem-loop` | Amber |
| `yesresearch` | `yesmem-research` | Teal |
| `subresearch` | `yesmem-subresearch` | Magenta (with its own status/menu tone) |
| `orchestrator` | `yesmem-dark` | Violet |

`subresearch` is a mapping for `subresearch-*` sections. The yesresearch skill
names its area agents `yesresearch-<topic>-<cluster>`, so they take the research
accent; name a section `subresearch-…` (or pass `theme`) to get magenta.

All four share one dark base (background, body text, borders), so the accent is
what carries the type. They are dark-only by construction; with `theme.mode: light`
in `cli.json`, opencode keeps its light surfaces for a few elements (its own
built-in dark themes behave the same way). The theme files ship with yesmem and
are installed to
`~/.config/opencode/themes/` by `yesmem update`, the setup wizard, and
`make deploy`; opencode discovers every `*.json` in that directory (and in any
`.opencode/themes/` between the working directory and `$HOME`). `yesmem uninstall`
removes the files it installed and leaves themes you added yourself alone. A
`yesmem-*` theme that is not installed is reported in the daemon log — opencode
itself falls back to its default theme for an unknown name without saying so.

Resolution order per spawn: the `theme` parameter of `spawn_agent` →
`agents.themes[<section prefix>]` → no theme (opencode's own default).

The daemon applies the resolved theme differently depending on the deployed
opencode binary: it passes `--theme <name>` when `opencode --help` documents that
flag (checked once per binary generation, keyed by mtime+size), otherwise it sets
`OPENCODE_CLI_CONFIG_CONTENT` for the spawned process. A theme name that could be
parsed as a flag is refused, since it would abort the start of the TUI. If that
variable is already set in the daemon's environment, the daemon leaves it alone —
an explicit operator setting wins over the agent theme. The one exception is
yesmem's own value (`{"theme":{"name":…}}` and nothing else): a daemon started
from inside an agent window inherits it, and reading that as an operator decision
would disable theming for good.

```yaml
agents:
  themes:
    yesloop: yesmem-loop
    yesresearch: yesmem-research
    subresearch: yesmem-subresearch
    orchestrator: yesmem-dark
    # Any opencode theme name works, including the built-in ones:
    # yesresearch: tokyonight
    # Disable theming for one type:
    # yesloop: ""
```

---

## forked_agents — Forked-Agent Proxy

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Enable forked-agent feature. Off by default. |
| `model` | string | `sonnet` | Forked-agent model (empty = same model as the main thread). |
| `token_growth_trigger` | int | `20000` | Token growth that triggers a fork. |
| `max_forks_per_session` | int | `50` | Max forks per session. `0` = unlimited. |
| `max_cost_per_session` | float64 | `5.0` | Max cost per session (USD). `0` = no limit. |
| `debug` | bool | `false` | Debug logging for forked-agent requests. |

A hardcoded absolute floor of 60000 tokens (`minTotalTokens` in `internal/proxy/proxy.go`) applies on top of `token_growth_trigger` and is not configurable.

---

## secrets_sanitization — Secret Redaction

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Enable secret redaction (LLM input/output + persisted bash outputs). |
| `allowed_exceptions` | []string | `[]` | List of strings that bypass redaction (full match, not substring). |

**Important:** `allowed_exceptions` matches the **entire** pattern match, not substrings. That rule also holds for the template patterns (`aws_secret_access_key`, `password_in_url`, `generic_api_key`), where only part of the match would be replaced: an exception is compared against the full match, and the allowlisted text is shielded from the remaining patterns. A later pattern can still match that text together with its surrounding context, which loses text but never leaks it.

---

## pricing — Pricing Table

Per-million-token pricing for budget tracking. Configurable without rebuild.

| Key | Type | Default (Input/Output) |
|-----|------|------------------------|
| `haiku` | `{input, output}` | 1.0 / 5.0 |
| `sonnet` | `{input, output}` | 3.0 / 15.0 |
| `opus` | `{input, output}` | 5.0 / 25.0 |
| `gpt-5-mini` | `{input, output}` | 0.25 / 2.0 |
| `gpt-5.2` | `{input, output}` | 1.75 / 14.0 |
| `gpt-5.4` | `{input, output}` | 2.5 / 15.0 |
| `deepseek-v4-flash` | `{input, output}` | 0.14 / 0.56 |
| `deepseek-v4-pro` | `{input, output}` | 0.28 / 1.12 |

---

## paths — Paths (optional)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `db` | string | `~/.claude/yesmem/yesmem.db` | Main database. |
| `bleve_index` | string | `~/.claude/yesmem/bleve-index` | Full-text index. |
| `archive` | string | `~/.claude/yesmem/archive` | Archive path. |
| `claude_projects` | string | `~/.claude/projects` | Claude Code project directory. |

---

## default_sandbox_profile

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `default_sandbox_profile` | string | `""` | Default sandbox profile: `none`, `standard`, `strict`. Empty = `standard`. |

---

> **See also:** `config.example.yaml` in repo root (abridged version with inline comments).

### exclude_projects — Project Exclusion

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `exclude_projects` | []string | `[]` | Projects to exclude from session indexing. Prevents noise directories from being tracked. |

### caps_dir — CAP Storage

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `caps_dir` | string | `~/.claude/caps/` | Directory for CAP.md capability files. Runtime-agnostic storage for reusable tool definitions. |
