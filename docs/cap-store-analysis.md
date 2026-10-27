# cap_store Analysis System

Queryable analysis layer on top of `cap_store` namespaced SQLite tables. Three composable capabilities turn any captured dataset (reddit comments, docs, bug reports, logs) into a searchable, summarizable, replayable knowledge base.

## Architecture

```
  REPL capability            daemon                    caps.db
  ----------------           ------                    -----------
  cap_search       ─── mcp__yesmem__cap_store ───→     cap_<name>__<table>
  cap_collect      ─── query / upsert / create_table    ├── posts
  cap_save_analysis    (via daemon RPC handler)         ├── comments
                                                        ├── links
                                                        └── analyses    ← new
```

All three caps are thin JS wrappers around the existing `mcp__yesmem__cap_store` RPC — no daemon changes needed to use them. Each capability-owned table lives under `cap_<capname>__<tablename>` in the shared `caps.db`.

## The Three Capabilities

| Cap | Purpose | Mode |
|---|---|---|
| `cap_search` | Find rows matching a WHERE clause. Auto-pagination via `all: true`. | sync |
| `cap_collect` | Pull rows for analysis. `mode: 'data'` returns rows to the REPL for in-chat summary; `mode: 'haiku'` calls Claude Haiku via curl. | sync / API |
| `cap_save_analysis` | Persist a summary to `cap_<cap>__analyses` with source-meta, instruction, tags. Auto-creates the table. | sync |

### Design principle

The calling Claude IS a summarizer — paying for an external Haiku roundtrip is only worth it for headless / cron / cost-averaged use. Default: `mode: 'data'` → rows flow back into the conversation, this Claude writes the summary, then saves it via `cap_save_analysis`. Zero extra API call, zero auth, zero latency.

## API Reference

### cap_search

```js
await cap_search({
  cap: 'reddit_fetch',           // required
  table: 'posts',                // required
  where: 'score > ? AND subreddit = ?',
  args: [100, 'LocalLLaMA'],
  limit: 100,                    // default 100, max 1000
  offset: 0,
  all: true,                     // auto-paginate until has_more=false
  max_rows: 2000                 // cap for all-mode (default 2000)
})
// → { rows, count, total, has_more, next_offset, truncated }
```

WHERE supports SQLite syntax. Quoted string literals are stripped before keyword-sanitization so `body LIKE '%DELETE%'` works (literal), while `; DROP TABLE` is rejected (statement-stacking).

### cap_collect

```js
// mode: 'data' — default, rows returned to chat
await cap_collect({
  cap: 'reddit_fetch',
  table: 'comments',
  where: 'score > ?',
  args: [5],
  instruction: 'optional, passes through to response',
  max_rows: 500                  // default 500
})
// → { mode: 'data', rows, count, total, source, instruction, truncated? }

// mode: 'haiku' — delegates summary to Claude Haiku via curl
await cap_collect({
  cap: 'reddit_fetch',
  table: 'comments',
  where: 'post_permalink = ?',
  args: ['https://reddit.com/...'],
  instruction: 'Fasse die Hauptkritikpunkte in 3 Sätzen',
  mode: 'haiku',
  model: 'claude-haiku-4-5-20251001',  // default
  max_tokens: 2000               // default 2000
})
// → { mode: 'haiku', summary, model, rows_used, total_in_db, source, key_source, usage }
```

API key source for haiku-mode:
1. `$ANTHROPIC_API_KEY` env var → `key_source: 'env'`
2. Fallback: `api_key:` field in `~/.claude/yesmem/config.yaml` → `key_source: 'config.yaml'`
3. Neither → error

### cap_save_analysis

```js
await cap_save_analysis({
  cap: 'reddit_fetch',           // required — which capability's data
  source_table: 'comments',      // required — which source table
  filter_where: 'post_permalink = ?',
  filter_args: ['https://...'],
  instruction: 'Was kritisieren die Kommentatoren?',  // required
  summary: 'Die Mehrheit bemängelt ...',              // required
  row_count: 12,
  model: 'claude-opus-4-7',      // default 'claude-opus-4-7'
  tags: 'tldr,llm,karpathy'      // comma-separated, optional
})
// → { id, cap, table: 'analyses', model, row_count }
```

Auto-creates `cap_<cap>__analyses` on first call. Safe idempotent — existing table is reused.

## Analyses Table Schema

Created automatically by `cap_save_analysis`. Domain columns:

| Column | Type | Purpose |
|---|---|---|
| `source_table` | TEXT | e.g. `comments`, `posts`, `links` |
| `filter_where` | TEXT | the WHERE clause used during collection |
| `filter_args` | TEXT (JSON) | the args array serialized |
| `instruction` | TEXT | the analysis question |
| `summary` | TEXT | the analysis result |
| `row_count` | INTEGER | how many rows were analyzed |
| `model` | TEXT | which model produced the summary |
| `tags` | TEXT | comma-separated free-form tags |

Auto-added by `cap_store.CapStoreCreateTable`:

| Column | Type | Purpose |
|---|---|---|
| `id` | INTEGER PK AUTOINCREMENT | row id |
| `created_at` | DATETIME DEFAULT CURRENT_TIMESTAMP | ISO 8601 timestamp |
| `updated_at` | DATETIME DEFAULT CURRENT_TIMESTAMP | ISO 8601 timestamp |

Do NOT include `id`, `created_at`, `updated_at` in user-provided `columns` — you will get `duplicate column name` on `create_table`.

## Retrieval Patterns

### By tag

```js
await cap_search({cap:'reddit_fetch', table:'analyses',
  where:"tags LIKE '%karpathy%'"})
```

### By topic (instruction OR summary)

```js
await cap_search({cap:'reddit_fetch', table:'analyses',
  where:"instruction LIKE '%LLM-Wiki%' OR summary LIKE '%knowledge-base%'"})
```

### By time range

```js
await cap_search({cap:'reddit_fetch', table:'analyses',
  where:"created_at >= ?", args:['2026-04-01']})
```

### By source filter (which posts were analyzed?)

```js
await cap_search({cap:'reddit_fetch', table:'analyses',
  where:"filter_where LIKE '%post_permalink%'"})
```

### Recursive meta-analysis

Analyses are just rows. Feed them back into `cap_collect`:

```js
await cap_collect({cap:'reddit_fetch', table:'analyses',
  where:"tags LIKE '%karpathy%' AND created_at >= ?",
  args:['2026-02-01'],
  instruction:'Finde wiederkehrende Themen über alle Karpathy-Analysen',
  mode:'haiku'})
```

This is the long-horizon payoff: after 100 sessions of analyses, ask "what patterns emerge?" across all of them.

## Auth Modes

| mode | API-Key user | Subscription user |
|---|---|---|
| `data` | works | works |
| `haiku` | works (env or config.yaml fallback) | requires explicit `ANTHROPIC_API_KEY` env-var setup |

Subscription users (Claude Code OAuth auth) have no exposed API key. `mode: 'haiku'` is not available out-of-the-box for them. Workaround: set `ANTHROPIC_API_KEY` manually, or stick with `mode: 'data'`.

Future: optional proxy-passthrough route for subscription-haiku — backlog.

## Edge Cases & Gotchas

### Column collision

`cap_store.CapStoreCreateTable` auto-adds `id`, `created_at`, `updated_at`. Don't pass them in your `columns` array.

### WHERE sanitizer strips literals first

`body LIKE '%DELETE%'` is legit (literal search) — passes. `; DROP TABLE` or `UNION SELECT` — blocked. Quoted-literal contents are stripped before keyword-checking.

### MCP schema cache

When you supersede a capability (`save_cap({supersedes: N})`), existing threads must call `mcp__yesmem__activate_cap({name: "<name>"})` again to pick up the new handler. The proxy caches per thread.

### Supersede pattern

Old capabilities aren't deleted — they're marked superseded. Query history:

```js
await mcp__yesmem__get_capabilities({name: 'cap_collect'})
// returns active version + superseded chain
```

### auto-pagination limit

`cap_search({all: true})` stops at `max_rows` (default 2000) to prevent accidental full-table scans on large caps. Set higher explicitly if needed.

### LIKE on analyses table

Works fine up to ~10k analyses. Beyond that, consider FTS5 index (future backlog).

## Lifecycle

1. `save_cap` — persist cap definition in the learnings DB
2. `activate_capability` (per thread) — daemon queues the registerTool snippet for proxy injection
3. Proxy inserts the snippet into the next user→assistant pair
4. REPL `registerTool` call makes the capability callable as a native tool
5. Subsequent calls use the capability directly

See `internal/proxy/caps_inject.go` and `internal/daemon/handler_caps.go` for implementation.

## Related

- `docs/cap-store-analysis-examples.md` — concrete end-to-end walkthroughs
- `internal/storage/cap_store.go` — underlying storage layer
- `internal/daemon/handler_cap_store.go` — RPC surface
