# Cap recipes

Working CAP.md templates per pattern. Copy, adapt, save_cap. Each section has when-to-use, complete CAP.md verbatim, and common mistakes for that pattern.

All examples assume Cap-Spec v1.1 (multi-script `## Scripts` with `### <name>` subsections). v1.0 single-`## Script` form is rejected by the parser. Cap-spec/examples/*/CAP.md in the upstream repo are still v1.0 — do not copy them blindly. See `gotchas.md` "Cap-spec format drift".

---

## 1. REPL-only single-script cap

**When**: One JS function. No bash, no scheduler, no auto-correct. The simplest case. 80 % of caps look like this.

**CAP.md** (`hn_top` — fetch top HN stories, persist queryable):

````markdown
---
name: hn_top
description: Fetch top N HackerNews stories and persist them into cap_hn_top__stories, queryable by score/title/date.
version: 1
tags: [web, hn, fetch, cap_store]
scope: user
auto_active: false
tested: true
---

## Purpose

Fetches the current top N stories from `hacker-news.firebaseio.com` and upserts them into `cap_hn_top__stories`. Each call appends a snapshot keyed by `fetched_at` — re-running gives you a history series, not a single overwrite.

Inputs: `limit` (integer, default 10).
Returns: `{count, stories: [{story_id, title, url, score, fetched_at}, ...]}`.

## Scripts

### hn_top
kind: tool

```javascript
async ({limit = 10}) => {
  const HN = 'https://hacker-news.firebaseio.com/v0';
  await mcp__yesmem__cap_store({
    capability: 'hn_top', action: 'create_table', table: 'stories',
    columns: JSON.stringify([
      {name:'story_id', type:'INTEGER'},
      {name:'title',    type:'TEXT'},
      {name:'url',      type:'TEXT'},
      {name:'score',    type:'INTEGER'},
      {name:'fetched_at', type:'INTEGER'}
    ])
  });
  const raw = await sh(`curl -s --max-time 15 "${HN}/topstories.json"`, 20000);
  const ids = JSON.parse(raw).slice(0, limit);
  const now = Math.floor(Date.now()/1000);
  const stories = [];
  for (const id of ids) {
    const s = JSON.parse(await sh(`curl -s --max-time 10 "${HN}/item/${id}.json"`, 12000));
    if (!s?.title) continue;
    const row = {story_id:id, title:s.title, url:s.url||'', score:s.score||0, fetched_at:now};
    await mcp__yesmem__cap_store({
      capability:'hn_top', action:'upsert', table:'stories',
      data: JSON.stringify(row)
    });
    stories.push(row);
  }
  return {count: stories.length, stories};
}
```

## Database

```sql
CREATE TABLE cap_hn_top__stories (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  story_id INTEGER,
  title TEXT,
  url TEXT,
  score INTEGER,
  fetched_at INTEGER,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```
````

**Common mistakes for this pattern**:

- Forgetting `await` on `sh(...)` → handler returns `[object Promise]` and downstream `.split`/`.trim`/`.map` crashes.
- Closure variables captured outside the arrow function don't survive `handler.toString()`. Inline constants like `const HN = '...'` go inside the body.
- Running `create_table` outside the handler (e.g. once at activation time): cap_store schema cache misses on fresh DBs. Always idempotent `create_table` first inside the handler.

---

## 2. Bash-handler cap with per-script sandbox override

**When**: Single bash script callable from the scheduler (cron) or as a tool. Per-script sandbox profile overrides the scheduled-job default.

**CAP.md** (`deploy_check` — minimal example: bash tool that runs in `strict` sandbox even if the scheduler default is `standard`):

````markdown
---
name: deploy_check
description: Quick deploy-status sanity check — runs git status + binary mtime in a strict sandbox, returns one-line summary.
version: 1
tags: [ops, deploy, check]
scope: user
tested: true
---

## Purpose

One-shot health check: is the worktree clean, when was the binary last built, is the daemon socket responding? Returns a single status line. Designed to run under cron via the scheduler with `auto_correct: false` (this is a check, not an action).

## Scripts

### deploy_check
kind: tool
runtime: bash
schema: {"type":"object"}
sandbox: strict

```bash
{ cd "${WORKTREE:-$HOME/projects/yesmem}" && git status --porcelain | head -1; } && stat -c '%y %n' ~/.local/bin/yesmem 2>/dev/null && test -S "${YESMEM_SOCK:-$HOME/.claude/yesmem/daemon.sock}" && echo OK || echo DEGRADED
```
```

````

**Notes**:

- `kind: tool` + `runtime: bash` REQUIRES a `schema:` line. Use `{"type":"object"}` for no-arg bash tools (parser otherwise rejects with `missing schema for bash tool`).
- `sandbox: strict` overrides the scheduled-job profile and the daemon default. Resolution order: per-script > job > daemon default.
- `scope: user` is required if you set `sandbox: none` (which this cap doesn't, but worth knowing). `scope: project` + `sandbox: none` is rejected by the parser.
- Single-line bash. No heredoc. Chain with `&&`/`;`. Output to stdout is the return value; non-zero exit signals error to the scheduler.
- A sandboxed run reaches only the restricted job socket, and `YESMEM_SOCK` points at it. Check `${YESMEM_SOCK:-$HOME/.claude/yesmem/daemon.sock}`: the full daemon socket is masked inside the sandbox. Under `sandbox: strict` the home is a tmpfs, so neither path exists there.

**Common mistakes**:

- Omitting `schema:` for a bash tool → parse error `missing schema for bash tool`.
- Putting `kind: handler` and then expecting it to be callable as a tool from chat → handlers are scheduler-only, not exposed to AI assistants.
- Forgetting timeouts inside the bash body — bash tools can hang the scheduler. Always use `--max-time` on curl, `timeout N` on long-running commands.

---

## 3. Multi-script cap with shared Database (cap_store tables)

**When**: Several related operations sharing the same tables and config. Telegram bots, deploy pipelines, multi-step workflows where each step is independently callable.

**CAP.md** (`telegram_bot` — single tool to send + a handler to poll updates, both reading the same config table):

````markdown
---
name: telegram_bot
description: Send Telegram messages and poll for replies; persist bot config and seen update_ids in cap_telegram_bot__*.
version: 1
tags: [messaging, telegram, notifications]
scope: user
requires: [store, web]
---

## Purpose

Three operations sharing one config table:
- `telegram_send` (tool) — send a message to a chat_id
- `telegram_poll` (handler, scheduler) — pull new updates, store unseen ones for later reply
- `telegram_reply` (handler, scheduler) — reply to unprocessed messages with a canned response or LLM-generated text

Setup: store the bot token via `### Setup` action below.

## Scripts

### telegram_send
kind: tool
schema: {"type":"object","properties":{"chat_id":{"type":"string"},"text":{"type":"string"}},"required":["chat_id","text"]}

```javascript
async ({chat_id, text}) => {
  const cfg = await mcp__yesmem__cap_store({
    capability:'telegram_bot', action:'query', table:'config',
    where:'key = ?', args: JSON.stringify(['bot_token'])
  });
  const token = cfg?.rows?.[0]?.value;
  if (!token) return {error: 'bot_token not configured — run Setup'};
  const url = `https://api.telegram.org/bot${token}/sendMessage`;
  const body = JSON.stringify({chat_id, text, parse_mode: 'Markdown'});
  const resp = await sh(
    `curl -s --max-time 15 -X POST -H 'Content-Type: application/json' -d ${shQuote(body)} ${shQuote(url)}`,
    20000
  );
  return JSON.parse(resp);
}
```

### telegram_poll
kind: handler
runtime: bash
sandbox: standard

```bash
TOKEN=$(yesmem cap-store telegram_bot query config "key = ?" '["bot_token"]' | jq -r '.rows[0].value // empty')
test -n "$TOKEN" || { echo '{"error":"no bot_token"}'; exit 1; }
LAST=$(yesmem cap-store telegram_bot query config "key = ?" '["last_update_id"]' | jq -r '.rows[0].value // 0')
curl -s --max-time 15 "https://api.telegram.org/bot${TOKEN}/getUpdates?offset=$((LAST+1))" | jq -c '.result[]?' | while read -r upd; do echo "$upd" | yesmem cap-store telegram_bot upsert messages; done
```

### telegram_reply
kind: handler
runtime: bash
sandbox: standard

```bash
yesmem cap-store telegram_bot query messages "processed = 0" '[]' | jq -c '.rows[]?' | while read -r row; do echo "process $row" >&2; done
```

## Database

```sql
CREATE TABLE cap_telegram_bot__config (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key TEXT UNIQUE,
  value TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE cap_telegram_bot__messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  update_id INTEGER UNIQUE,
  chat_id TEXT,
  from_user TEXT,
  text TEXT,
  processed INTEGER DEFAULT 0,
  received_at INTEGER,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

## Actions

### Setup

1. Open [@BotFather](https://t.me/BotFather), send `/newbot`, copy the token.
2. Store it from REPL:

```javascript
await mcp__yesmem__cap_store({
  capability:'telegram_bot', action:'upsert', table:'config',
  data: JSON.stringify({key:'bot_token', value:'<YOUR_TOKEN>'})
});
```

3. Schedule the poll/reply handlers if you want async replies:

```javascript
await mcp__yesmem__schedule({action:'create', name:'tg_poll', interval_seconds:15, mode:'bash', cap_name:'telegram_bot'});
```
````

**Common mistakes**:

- Calling a `kind: handler` from chat — handlers aren't tool-registered. Only `kind: tool` scripts get an MCP entry. Handlers fire only via the scheduler.
- Forgetting that all scripts in a cap share the SAME `## Database` and the SAME cap_store namespace. Tables don't get namespaced per script.
- Mixing `kind: handler` with a `schema:` field — parser rejects with `invalid schema on handler`.
- Different scripts with different sandbox profiles inside one cap is fine. Different `runtime` per script is fine. Different `kind` per script is fine. Same `name` twice in one cap is rejected.

---

## 4. Cap with auto_active + project scope

**When**: A cap that should auto-load every session, but only inside one specific project (e.g. project-specific deploy or test runner).

**CAP.md** (`worktree_status` — auto-active inside the yesmem feature branch only):

````markdown
---
name: worktree_status
description: Show current yesmem worktree status (branch, dirty files, last commit) inline. Auto-active in the yesmem project.
version: 1
tags: [git, worktree, ops]
scope: project
auto_active: true
tested: true
---

## Purpose

Drop-in replacement for typing `git status -s && git log -1 --oneline` over and over. Auto-injected in this project so it shows up as a tool without `activate_cap`.

Returns `{branch, dirty, last_commit}`.

## Scripts

### worktree_status
kind: tool

```javascript
async () => {
  const branch = (await sh('git rev-parse --abbrev-ref HEAD')).trim();
  const dirty  = (await sh('git status --porcelain')).split('\n').filter(Boolean);
  const last   = (await sh('git log -1 --oneline')).trim();
  return {branch, dirty: dirty.length, dirty_files: dirty, last_commit: last};
}
```
````

**Notes**:

- `scope: project` means the cap is only available when working inside the matching project. The CAP.md must NOT use `sandbox: none` on any script — parser will reject. Use `sandbox: standard` or omit (omitting is fine; default is `standard`).
- `auto_active: true` + `scope: project` = auto-injected only when the project context matches.
- `auto_active: true` + `scope: user` = auto-injected globally. Use sparingly — every session pays the token cost in the system prompt.

**Common mistakes**:

- `scope: project` + `sandbox: none` → parser error `script "X": sandbox=none not allowed on scope=project caps (use scope=user)`. To bypass sandbox you must move scope to `user`.
- Setting `auto_active: true` for an experimental cap. Promote to `auto_active: true` only after the cap proves its universal value. Easy to retract via supersede, but you pay tokens until you do.

---

## 5. Cap that needs capblob-pipe (output > 30 KB)

**When**: A cap fetches or produces output bigger than the ~30 KB `sh()` stdout truncation wall. Reddit threads, GitHub API dumps, paginated search results.

The pattern: pipe straight from `curl` into `blob_put` (a CLI binding the daemon exposes). Then read back chunks via `cap_store query`. No intermediate /tmp file, no Read-tool prompt.

**CAP.md fragment** (script body of `reddit_fetch` — shows the blob-pipe call and chunk-read loop):

````javascript
async ({url, max_comments}) => {
  if (!url || typeof url !== 'string') return {error: 'url required'};
  url = url.replace(/^reddit:/i, '').trim().replace(/\/$/, '');
  if (!/^https?:\/\/(www\.|old\.)?reddit\.com\//i.test(url))
    return {error: 'not a reddit URL', given: url};

  const fetchUrl = url + '.json?limit=500&raw_json=1';
  const key = 'url:' + url;

  // Pipe curl stdout directly into blob storage — bypasses the 30 KB wall
  const putRes = await sh(
    `curl -sL -A "YesMem/1.0" ${shQuote(fetchUrl)} --max-time 15`
    + ` | yesmem cap-blob-put --cap reddit_fetch --key ${shQuote(key)}`,
    20000
  );
  if (!String(putRes).includes('"status":"ok"'))
    return {error: 'blob_put failed', detail: String(putRes).slice(0, 400)};

  // Read back chunks. cap_store responses cap ~25 KB per query, so loop.
  let rows = [];
  for (let i = 0; i < 50; i++) {
    const r = await mcp__yesmem__cap_store({
      capability: 'reddit_fetch', action: 'query', table: 'blobs',
      where: 'key=? AND chunk_idx=?',
      args: JSON.stringify([key, i]),
      limit: 1
    });
    const arr = (typeof r === 'string' ? JSON.parse(r) : r).rows || [];
    if (!arr.length) break;
    rows.push(arr[0]);
  }
  if (!rows.length) return {error: 'blob empty after put', key};

  const raw = rows.map(r => r.data || '').join('');
  const data = JSON.parse(raw);   // parse the reassembled payload
  // ... extract, persist to other tables, return small summary ...
}
````

**Schema for blobs table** (auto-created on first `cap-blob-put`, but document it for the cap):

````sql
CREATE TABLE cap_<name>__blobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key TEXT,
  chunk_idx INTEGER,
  data TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
````

**Cleanup before re-fetching the same key**:

```javascript
await mcp__yesmem__cap_store({
  capability:'my_cap', action:'delete', table:'blobs',
  where:'key=?', args: JSON.stringify([key])
});
// then pipe new data
```

**When NOT to use capblob**:

| Situation | Do instead |
|---|---|
| Transient HTML you parse once and discard | Server-side parse: `curl ... \| python3 -c '<parser>'` → upsert only the extracted 1-3 KB row. Never persist raw HTML you won't query. |
| Output < 25 KB | Plain `sh()` is simpler. |
| Data you want to `LIKE`-search on | Blobs aren't queryable. Parse to columns first. |

**Common mistakes**:

- Reaching for capblob because a URL *might* yield > 30 KB. If you only need three fields, parse server-side and skip the blob layer entirely.
- Forgetting to `delete WHERE key=?` before re-fetching → chunks accumulate forever.
- Reading back without checking `arr.length === 0` to break the loop → infinite loop on missing data.
- Single-query read-back of a 60 KB blob: cap_store responses cap at ~25 KB per query (silent truncation). Always page chunk-by-chunk via `chunk_idx`.

---

## 6. Bundled cap (lives in `caps/bundled-caps/<name>/CAP.md`, installed by `yesmem migrate`)

**When**: A cap that ships with yesmem itself, available to every install. `cap_search`, `cap_collect` and `changelog_check` are bundled.

**Layout**:

```
caps/bundled-caps/<name>/CAP.md         ← in repo (the bundled source; the manifest decides whether it may replace the installed body)
caps/bundled-caps/manifest.yaml         ← in repo, one derived_from_hash per cap: the body each bundle was cut from
~/.claude/caps/<name>/CAP.md             ← installed copy (default caps dir, daemon reads this at runtime)
~/.claude/caps/manifest.yaml             ← the installation's record: the digest of every body the installer delivered
caps.db (learnings row, category="cap")  ← daemon-managed, written back to disk on restart
```

**Lifecycle**:

1. Edit `caps/bundled-caps/<name>/CAP.md` in the repo. `version:` is display only, it gates nothing and only tells the human reader what they are looking at; the manifest entry already names the body your new one descends from, so a normal edit needs no second adjustment.
2. `make deploy` builds the binary and swaps `~/.local/bin/yesmem`. It installs no cap at all: measured 2026-10-23, the rocketchat cap stood at v15 before and after a deploy.
3. `yesmem migrate` runs `InstallBundledCaps()` (`internal/setup/setup.go`); `yesmem update` reaches the same path once it has actually installed a release. The installer decides from `derived_from_hash`, never from `version:`. No entry and nothing on disk: install. Entry naming the installed body: the bundle is its successor, install, whatever `version:` says. Entry naming a different body: keep the installed body and print one conflict line with both hashes plus the hash to record.
4. The daemon imports the file at its next start, and a changed `CAP.md` is re-imported by the 30s watcher (`internal/daemon/daemon.go`). The import gate refuses a version below `cap_imported_version`, the revision the row was imported from, unless the installation's manifest names the file that arrived: that body was delivered on purpose, the row adopts its revision. A row that carries a repair on top of the body it imported is the later state, and the delivery is refused and reported.
5. On daemon restart, the **DB row is written back to disk**, but not over a delivery the row's own source is behind. Keeping the file is logged with the row's version and the revision it was imported from. Where the manifest names something else, the row still wins on its counter and the write is logged with both digests.

**Dual-write workflow when the live row is ahead of your edit**:

```javascript
// 1. Edit caps/bundled-caps/<name>/CAP.md in the repo

// 2. If the installation's budget of revisions is what you cannot move (an old
//    install, a row that no manifest entry vouches for), ALSO call save_cap with the
//    same body, from the REPL
const body = await Read({file_path: '/path/to/repo/caps/bundled-caps/my_cap/CAP.md'});
// (parse out the script body, build the scripts array)

await mcp__yesmem__save_cap({
  name: 'my_cap',
  description: '...',  // match frontmatter description
  scripts: JSON.stringify([{
    name: 'my_cap', kind: 'tool', runtime: 'repl',
    body: extractedHandlerSource,
    schema: extractedSchemaJSON
  }]),
  tags: 'wiki,export',
  tested: true,
  test_date: '2026-05-01',
  auto_active: true   // match frontmatter
});

// 3. the export writes the row back to the installed copy when the row's version is above it and the row is the later state (a repair on top of the body it imported); `make deploy` is not part of cap delivery
```

**Diagnostic when the digests disagree**:

```bash
sha256sum caps/bundled-caps/<name>/CAP.md
sha256sum ~/.claude/caps/<name>/CAP.md
grep -A2 "name: <name>$" ~/.claude/caps/manifest.yaml   # the body this installation was given
grep '^version:' ~/.claude/caps/<name>/CAP.md
```

`derived_from_hash` is `textutil.BodyDigest`, not plain sha256: it ignores line endings and trailing newlines and keeps everything else. To compare by hand, strip CR and trailing newlines first (`sed 's/\r$//' file | ...`), or trust the conflict line, which prints both digests. If the installed body matches `derived_from_hash`, the installation is intact and the repo file is the older state. If it does not, either the body was edited by hand or the row wrote it back; `save_cap` is the way to make such a body official.

**Common mistakes**:

- Editing `caps/bundled-caps/<name>/CAP.md` and running `make deploy`, then finding the disk file unchanged. Nothing is broken: the deploy installs no cap. The install step is `yesmem migrate`, and it decides from `derived_from_hash`, not from `version:`.
- Editing `~/.claude/caps/<name>/CAP.md` directly. Daemon overwrites it on restart. Always edit the source-tree file plus `save_cap`.
- Forgetting the manifest entry after reconciling a conflict. The installer compares against the entry, so an unchanged entry reports the same conflict on every run. The conflict line names the digest to record.
- Trusting a subagent's summary "no save_cap needed for bundled-cap update" without checking whether the installation's manifest names the body. With an entry naming the live body, the bundle installs on its own; without one, `save_cap` is the only path.

---

## 7. Cap that pipes `yesmem query` into `yesmem json` (read-only DB → projected JSON)

**When to use**: A cap that needs to read across the daemon DBs (`yesmem.db`, `messages.db`, `caps.db`, `runtime.db`) and project a small JSON shape. The bash-runtime equivalent of writing a JS handler that calls `cap_store(action: 'query')` — but for tables outside your cap's own namespace.

**Why not raw `sqlite3`**: `yesmem query` opens the DB read-only, returns JSON, and survives daemon-side schema migrations. `sqlite3` direct would lock against the daemon and bypass projection rules.

```yaml
---
name: recent_gotchas
version: 1
description: List the N most recent unresolved gotcha learnings as compact JSON.
scope: project
auto_active: false
tags: [diagnostics,read-only]
---

## Purpose

Surface fresh `category='gotcha'` learnings (active, not superseded) for the current project without spawning a full search call. Useful at session start and when triaging "what broke recently".

## Database

No tables. This cap reads existing learnings via `yesmem query`; it owns no `cap_*` storage.

## Scripts

### recent_gotchas

```yaml
kind: tool
runtime: bash
schema: {"type":"object","properties":{"limit":{"type":"integer","default":10,"minimum":1,"maximum":100},"project":{"type":"string"}},"required":[]}
```

```bash
set -euo pipefail
LIMIT="${ARG_limit:-10}"
PROJECT="${ARG_project:-}"
case "$LIMIT" in *[!0-9]*|"") echo '{"error":"limit must be int"}'; exit 0;; esac
[ "$LIMIT" -gt 100 ] && LIMIT=100
WHERE="category='gotcha' AND superseded_by IS NULL"
if [ -n "$PROJECT" ]; then
  ESC="${PROJECT//\'/\'\'}"
  WHERE="$WHERE AND project='$ESC'"
fi
yesmem query --db yesmem --format objects \
  "SELECT id, project, substr(content, 1, 240) AS preview, created_at FROM learnings WHERE $WHERE ORDER BY id DESC LIMIT $LIMIT" \
| yesmem json '{count: length, items: [.[] | {id, project, preview, age_days: (((now - (.created_at|fromdateiso8601))/86400)|floor)}]}'
```
```

**Common mistakes for this pattern**:

- Forgetting `--format objects` — default `matrix` gives `{columns:[…], rows:[[…]]}` which gojq can't pivot easily.
- Concatenating user input directly into the SQL. `yesmem query` does **not** support bind parameters — sanitize at the bash layer (single-quote escape, integer-only checks) before interpolating, and never accept arbitrary `WHERE` clauses from callers.
- Trying to `--slurpfile` external data into `yesmem json`. Not supported. Build the auxiliary JSON inside the SQL (`json_group_array`, `json_object`) or pipe two `yesmem query` calls together.
- Using a function parameter named `label` inside `yesmem json` — gojq reserves that keyword. Use `lbl`/`name`/`key`.
- Reading from a table in another DB without switching `--db`. The error `no such table: messages` means the table lives in `messages.db`, not `yesmem.db`.

---

## Cross-cutting reminders

- **Always test in REPL before save_cap.** Run the handler with real inputs. Verify output shape, persistence, idempotency, error paths.
- **Always re-`activate_cap` open threads after a supersede.** Proxy caches the per-thread tool list.
- **Always include timeouts.** `sh(cmd, 20000)`, `curl --max-time 15`. Hung handlers block the REPL or scheduler tick.
- **Always treat `cap_store` `where` as untrusted-input territory.** Use `?` placeholders + `args` array. Don't concatenate user input into raw WHERE — see `gotchas.md` "sanitize_where blocked keywords".
