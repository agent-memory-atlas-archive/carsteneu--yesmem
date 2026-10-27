# Cap-builder gotchas

Concrete pitfalls, sorted by where they bite. Each entry: symptom, root cause, fix, and a stage-1 reference where applicable.

Format: stage-1 references use `[cc0ba29d msg:N]`, `[cc0ba29d-p2 msg:N]`, `[bb37bd60 msg:N]` to point to the verbatim source quote in `../session-*.md`.

---

## save_cap

### save_cap PLACEHOLDER bug — body stored as literal string

**Symptom**: `get_caps({name})` shows `body: "PLACEHOLDER"`. `activate_cap` returns a `code` field that builds a tool, but calling the tool fails or returns nonsense.

**Root cause**: When the `scripts` JSON exceeds ~10 KB and is passed as a literal inline argument in an MCP tool_use call, the argument-substitution path silently stores the placeholder token instead of the real body. The daemon never sees the actual JSON.

**Fix**: Build the JSON in a REPL variable first, then call save_cap from REPL:

```javascript
const body = await Read({file_path: '/path/to/handler.js'});
const scripts = JSON.stringify([{
  name: 'my_cap', kind: 'tool', runtime: 'repl',
  body: body, schema: JSON.stringify({type:'object'})
}]);
await mcp__yesmem__save_cap({name: 'my_cap', description: '...', scripts});
```

**Verification recipe** (after save):

```bash
sqlite3 ~/.claude/yesmem/yesmem.db "SELECT id, length(context), instr(context,'<KNOWN-BODY-PREFIX>')>0 AS has_body, instr(context,'PLACEHOLDER')>0 AS has_placeholder FROM learnings WHERE name='<cap_name>' AND superseded_by IS NULL"
```

Both `has_body=1` and `has_placeholder=0` are required. The body lives in `learnings.context`, not `learnings.content`. The daemon may emit a second auto-supersede row ~4 seconds after the user save with ~9 bytes length delta (re-encoding) — both rows must show `has_body=1`. Length deltas <50 bytes are normal re-encoding, not body loss.

**Stage-1**: [bb37bd60 msg:248-252, msg:296], [cc0ba29d pinned-gotcha].

### CONTRADICTS SKILL — save_cap takes `scripts:` not `handler_repl`/`handler_bash`

**Symptom**: Following the older skill's hn_top example, save_cap "succeeds" but the cap structure on disk doesn't match the v1.1 multi-script format, sandbox per-script overrides don't work, and bash tools without explicit schema get rejected on parse.

**Root cause**: The legacy `handler_repl` / `handler_bash` top-level fields are still accepted by `scriptsFromSaveCapParams` for backward compatibility, but every new feature (multi-script bundles, per-script kind/runtime, per-script sandbox, explicit script schema) requires the `scripts:` JSON-array shape.

**Fix**: Always use `scripts: JSON.stringify([{name, kind, runtime, body, schema}])`. See `api-reference.md`.

**Stage-1**: [bb37bd60 msg:248, 255, 951 — verbatim save_cap calls].

### REPL globals don't survive hook-inject blocks

**Symptom**: A 10 KB `o.b64` global set in turn N is `undefined` in turn N+1. Workflows that build a body in one turn and save in the next silently fail.

**Root cause**: REPL globals (`o.f1`, `o.f2`, simple paths, function references) do **not** reliably survive a hook-inject block between two REPL calls — the next call sees them as `undefined`. Large strings (>10 KB on `o.*`) fail this even more often. The clearing happens silently; you only notice when a follow-up `sh()`/`cat()`/`Edit()` call references the missing variable and fails with `... undefined: Ist ein Verzeichnis` or similar.

**Fix**: Do everything in one REPL call. Build, test, and save_cap in a single block. Don't split across turns when the body is large.

**Stage-1**: [bb37bd60 msg:296 "REPL-State über Turns: o.b64 (10KB String) verschwand zwischen Calls"].

---

## REPL VM

### REPL VM allowlist hallucinations

**Symptom**: Cap activates fine but crashes on first call with `ReferenceError: file is not defined` or similar.

**Root cause**: The REPL VM does not have `process`, `require`, top-level `fetch`, `globalThis.fetch`, `axios`, `node-fetch`, or a `file()` global. Cap-Spec defines `file()` as an adapter primitive, but the YesMem provider implements it via `GenerateAdapterJS` ONLY if the script uses `file(` / `web(` / `store(` syntax — otherwise the adapter prefix is skipped.

**Fix**: Use the runtime builtins directly. Allowlist:

| Wrong | Right |
|---|---|
| `file({action:'write', path, content})` | `put(path, content)` or `await Write({file_path, content})` |
| `process.env.HOME` | `(await sh('echo $HOME')).trim()` |
| `require('fs')`, `import x from 'y'` | use `sh`, `cat`, `put` |
| `JSON.parse(sh(...))` (missing await) | `JSON.parse(await sh(...))` |
| `globalThis.fetch`, `axios` | `sh('curl -s URL')` |

**Pre-save sweep**:

```bash
grep -nE 'sh\(|file\(|process\.|require\(|import |globalThis|fetch\(' <handler-text>
```

**Stage-1**: [cc0ba29d msg:140-160 (allowlist table)], [bb37bd60 msg:296].

### sh() 30 KB stdout truncation — silent

**Symptom**: `JSON.parse(await sh('sqlite3 ... -json'))` throws `Unterminated string`. The underlying file is fine; the truncation happens at the REPL boundary.

**Root cause**: `sh()` truncates stdout return at ~30 KB even though the underlying command's output is much larger. Bench: `sqlite3 -json` producing 1.36 MB on disk yields only 29725 bytes back, mid-string.

**Fix**: Three options.

1. Redirect to /tmp via sh, then `cat()` or `Read({file_path})`:
   ```javascript
   await sh(`sqlite3 -json ${db} "${q}" > /tmp/out.json`, 20000);
   const raw = await cat('/tmp/out.json');
   ```
2. Keep the entire pipeline (sqlite|jq|bash) inside ONE `sh()` invocation that only returns small status counters:
   ```javascript
   const status = await sh('bash /tmp/render.sh && echo {"ok":true}', 60000);
   ```
3. Use the capblob-pipe pattern (`yesmem cap-blob-put`) for large remote payloads.

**Stage-1**: [bb37bd60 msg:60 search hit #59581], [cc0ba29d Pivot section].

### sh() returns wrapper object for large strings — must coerce via String()

**Symptom**: After `await sh(...)`, `result.includes(...)` or `result.split(...)` throws or returns wrong shape.

**Root cause**: For large outputs, sh() returns a wrapper object rather than a plain string. Direct string operations on it misbehave.

**Fix**: Coerce explicitly:

```javascript
const raw = String(await sh(cmd, 20000));
if (raw.includes('"status":"ok"')) { ... }
```

**Stage-1**: [bb37bd60 msg:180 "sh() liefert Wrapper-Objekt für große Strings. Coerce via String()"].

### put() works to ~120 KB but hangs at 4 MB

**Symptom**: A handler that builds a large script in JS and writes it via `put('/tmp/x.sh', script)` hangs the session or returns "Prompt is too long".

**Root cause**: put() works to ~120 KB but the LLM-output-budget path can't emit 4 MB content. The Anthropic API rejects with `Prompt is too long` because the assistant message holding the 4 MB tool-arg payload exceeds 1M tokens.

**Fix**: Don't render giant scripts in JS. Use the embedded-base64 pattern (script is in cap source, decoded at runtime to /tmp), or do heavy lifting shell-side and return only a small JSON status.

**Stage-1**: [bb37bd60 msg:3, 12, 296].

### Per-tool-call hook latency: 8-20 s × N → don't loop put() / sh()

**Symptom**: Cap that loops `put()` for N files takes hours instead of minutes.

**Root cause**: Every REPL tool call (`put`, `sh`, `Read`, etc.) goes through PreToolUse + PostToolUse hooks (matcher `.*`) which run daemon RPCs, embedding lookups, think-hint generation. Bench: `put: [8310, 8132, 15919]ms`, `sh: [17372, 19877, 7805]ms`. 1500 puts × ~12 s = ~5 hours.

**Fix**: Script-on-disk pattern. JS builds a single bash script that loops in shell-side, one `put()` writes the script, one `sh()` executes it. 1500 files in ~30 s instead of 5 hours.

**Stage-1**: [cc0ba29d msg:3971, 3996], [bb37bd60 msg:296].

---

## cap_store

### sanitize_where blocked keywords

**Symptom**: A WHERE clause containing `body LIKE '%delete me%'` works, but `delete from blobs` is rejected with `WHERE clause contains blocked SQL keyword`.

**Root cause**: `sanitizeWhere` (in `internal/storage/cap_store.go`) strips quoted string literals (`'...'`) first, then scans for `(?i)\b(UNION|ATTACH|DROP|ALTER|PRAGMA|CREATE|INSERT|UPDATE|DELETE|GRANT|REVOKE|SELECT)\b|;` in what remains. Keyword-in-literal is fine; keyword-as-statement (statement-stacking, cross-cap leak via `SELECT FROM pragma_table_info`) is blocked.

**Fix**: Use `?` placeholders + `args` JSON array. Don't concatenate untrusted input into raw WHERE.

```javascript
// Wrong
where: `name = '${userInput}'`
// Right
where: 'name = ?', args: JSON.stringify([userInput])
```

**Stage-1**: source: `internal/storage/cap_store.go:43, 59`.

### Auto-added columns must NOT be in your columns list

**Symptom**: `create_table` fails with `duplicate column name: id` (or `created_at` / `updated_at`).

**Root cause**: cap_store auto-adds `id INTEGER PRIMARY KEY AUTOINCREMENT`, `created_at DATETIME DEFAULT CURRENT_TIMESTAMP`, `updated_at DATETIME DEFAULT CURRENT_TIMESTAMP` to every table. Listing them yourself yields a SQL duplicate-column error.

**Fix**: Omit them from the `columns` array. Specify only domain columns.

**Stage-1**: source: `internal/storage/cap_store.go`, current skill.

### CapsMaxCellBytes = 64 KB per cell

**Symptom**: An upsert of a row with one big TEXT field fails with `cell "X" exceeds 65536 byte limit`.

**Root cause**: Per-cell limit is 64 KB (`CapsMaxCellBytes` in `internal/storage/cap_store.go:14`).

**Fix**: For payloads >64 KB, use the blob-pipe pattern (chunks in `cap_<name>__blobs`).

**Stage-1**: source: `internal/storage/cap_store.go:14, 269`.

### 10 tables per cap quota

**Symptom**: Eleventh `create_table` call fails with `quota exceeded: max 10 tables per cap`.

**Root cause**: `CapsMaxTablesPerCap = 10` in `internal/storage/cap_store.go:12`.

**Fix**: Consolidate into fewer wider tables, or split into multiple caps.

**Stage-1**: source: `internal/storage/cap_store.go:12, 168-169`.

### CapsMaxRowsPerTable = 10 000

**Symptom**: After 10 000 upserts, new inserts fail.

**Root cause**: `CapsMaxRowsPerTable = 10000` in `internal/storage/cap_store.go:13`.

**Fix**: Periodic prune via `delete WHERE created_at < ?`. cap_store is for working sets, not historical archives.

### cap_store vs cap_<name>__<table> prefix mismatch

**Symptom**: `cap_store({capability:'my_cap', table:'cap_my_cap__hits'})` returns `table not found`.

**Root cause**: cap_store's `table` parameter takes the SHORT name. The daemon adds the `cap_<capname>__` prefix automatically.

**Fix**: Pass `table:'hits'`, not `table:'cap_my_cap__hits'`. The fully-qualified name is only used in raw SQL inside `## Database` fences and in `sqlite_master` queries.

**Stage-1**: [cc0ba29d msg:1200 "cap_store tables use capability name as namespace prefix..."].

### cap_store response capped at ~25 KB per query

**Symptom**: A query returning a 60 KB blob chunk returns empty or truncated.

**Root cause**: cap_store responses cap at ~25 KB per single query (silent read-back cliff). Aggregate cap ~200 KB across pages.

**Fix**: Paginate via `offset` (`limit:1` per chunk for blobs, larger pages for normal rows). LIKE-search remains fast up to ~10k rows/table.

**Stage-1**: current skill "Known Limits".

---

## Naming and validation

### validateCapName: lowercase, underscores, no dashes

**Symptom**: `save_cap({name:'My-Cap'})` fails with `invalid name "My-Cap": must match [a-z][a-z0-9_]{0,63}`.

**Root cause**: `validateCapName` enforces `^[a-z][a-z0-9_]{0,63}$` for cap names AND table names AND column names AND data keys.

**Fix**: lowercase letter start, lowercase letters / digits / underscores, max 64 chars. No dashes, no camelCase, no leading digits.

**Stage-1**: source: `internal/storage/cap_store.go:46-54`.

---

## CAP.md format / parser

### v1.0 single-`## Script` form is rejected — there is no fallback

**Symptom**: An old CAP.md with `## Script` (singular) and cap-level `runtime:` / `schema:` in frontmatter fails to parse with `## Scripts section missing`.

**Root cause**: Cap-Spec v1.1 is the only supported version. The parser explicitly rejects v1.0 — no auto-migration, no fallback.

**Fix**: Migrate to v1.1: wrap the body in `## Scripts` / `### <cap_name>` subsection. Move `runtime:` and `schema:` from cap-level frontmatter into the script's metadata block. Keep `name` / `description` / `version` / `tags` / `scope` / `auto_active` at cap-level.

**Stage-1**: cap-spec/spec.md "Migration from v1.0".

### Cap-spec format drift: spec.md is v1.1, examples are v1.0

**Symptom**: Copying `cap-spec/examples/reddit_fetch/CAP.md` and renaming yields a parse failure `## Scripts section missing` because the example still uses the v1.0 `## Script` form.

**Root cause**: spec.md was migrated to v1.1-draft; examples/*/CAP.md and README.md were not. Format drift inside the cap-spec repo.

**Fix**: Use `caps/bundled-caps/cap_collect/CAP.md` (in this repo, freshest v1.1 reference) as the template instead of the upstream cap-spec examples. When copying from upstream, update to v1.1 first.

**Direction is fixed, not open**: the v1.1 parser is the only one that exists; v1.0 is rejected with `## Scripts section missing` and there is no fallback path. Migration of `examples/*` and `README.md` to v1.1 is the only viable resolution; reverting `spec.md` is not on the table because the codebase already shipped the v1.1 parser. Treat the upstream `examples/*` as historical until the migration PR lands — see Spec feedback item 1 below.

**Stage-1**: hook gotcha "cap-spec repo has format drift".

### `kind: tool` + `runtime: bash` REQUIRES schema

**Symptom**: Bash-tool cap fails to parse with `missing schema for bash tool`.

**Root cause**: For `kind: tool` with `runtime: bash`, the parser requires `schema:` because there's no JS function signature to derive from.

**Fix**: For no-arg bash tools, use `schema: {"type":"object"}`. For arg-taking tools, write the JSON Schema explicitly on a single line.

**Stage-1**: hook gotcha "kind:tool with runtime:bash REQUIRES a schema field".

### `kind: handler` MUST NOT have a schema

**Symptom**: Handler with `schema:` line fails to parse with `invalid schema on handler`.

**Root cause**: Handlers are scheduler-only; not invoked with structured arguments. Setting `schema:` is a parse error.

**Fix**: Remove the `schema:` line from handler subsections.

**Stage-1**: cap-spec/spec.md "Validation" table.

### schema must be single-line JSON

**Symptom**: A multi-line YAML schema (carried over from v1.0 cap-level frontmatter) parses partially. The metadata block ends at the first blank line / prose / fence, so subsequent schema lines are ignored or trigger "unknown metadata key".

**Root cause**: The metadata block consists of contiguous `key: value` lines. The first blank line, prose line, or code fence ends it. `schema:` must fit on one line.

**Fix**: Flatten to single-line JSON. For complex schemas, derive from the JS signature (REPL only) or split the script into multiple smaller scripts.

**Stage-1**: hook gotcha "In cap-spec v1.1 script-level metadata blocks, the schema field must be single-line JSON".

### `## Database` fence comments are rejected

**Symptom**: `## Database` with `-- stateless cap` inside the ```sql fence yields a parser error.

**Root cause**: The ```sql fence accepts only `CREATE TABLE/INDEX/VIEW/TRIGGER IF NOT EXISTS`. SQL comments inside the fence are rejected by the validator.

**Fix**: For stateless caps, leave the fence body completely empty or omit `## Database` entirely. The absence of the heading signals stateless.

**Stage-1**: current skill "Step 4.5 (c) Schema-Cross-Check".

### `scope: project` + `sandbox: none` rejected

**Symptom**: Save fails or activate fails with `script "X": sandbox=none not allowed on scope=project caps (use scope=user)`.

**Root cause**: `internal/capfile/parse.go:128-138` enforces this guard — project-scoped caps cannot bypass sandbox.

**Fix**: Either move scope to `user` (cap available globally) or use `sandbox: standard` / `sandbox: strict` (or omit; default is standard).

**Stage-1**: source: `internal/capfile/parse.go:120-145`, commit `b5481cf`.

---

## Bundled-cap lifecycle (CONTRADICTS SKILL)

### DB write-back overrides disk edit on daemon restart

**Symptom**: Edit `caps/bundled-caps/<name>/CAP.md`, run `make deploy` and `yesmem migrate`, and the repo edit never appears in the installed copy. `sha256sum` diverges, and `grep '^version:' ~/.claude/caps/<name>/CAP.md` shows a HIGHER number than the source file.

**Root cause**: The installed copy and the DB row carry their own revision counter, and both gates compare against it. A `save_cap` (or an auto-correct repair) previously stamped a higher version (e.g. v52) into the DB, so the installer keeps the installed v52 body and the self-written v47 edit stays unshipped. The repo file itself is never touched; the divergence is between the repo source and the installed copy.

The previous skill said "edit CAP.md + redeploy is enough". That's true only when the DB version equals or trails the source version. Once supersede chains kick in, you need both edits.

**Fix**: When updating a bundled cap, do BOTH:

1. Edit `caps/bundled-caps/<name>/CAP.md` in the repo.
2. Call `save_cap` from REPL with the same body.

Then `make deploy`. Both source and DB now agree.

**Diagnostic**:

```bash
sha256sum caps/bundled-caps/<name>/CAP.md
sha256sum ~/.claude/caps/<name>/CAP.md
grep '^version:' ~/.claude/caps/<name>/CAP.md
grep '^version:' caps/bundled-caps/<name>/CAP.md
```

If disk SHA != source SHA AND disk version > source version, the daemon wrote back. Fix via `save_cap`.

**Stage-1**: [bb37bd60 msg:828-948 "Verstanden — die DB überschreibt die Datei beim Daemon-Restart"], [cc0ba29d Storage section "Bundled-cap edit pipeline collides with DB-source-of-truth"].

### Pre-commit version sync (avoid same-day working-tree dirty)

**Symptom**: You edit `caps/bundled-caps/<name>/CAP.md` (e.g. body change), call `save_cap`, commit. On the next session start the working tree shows a diff in the `version:` frontmatter line — even though you committed minutes ago.

**Root cause**: `save_cap` auto-supersedes both the `learnings` row (id N → N+1) **and** the `version:` field embedded in the `scripts` JSON. If you committed a source `version: 55` while the DB now holds v56 from the supersede, the next daemon restart writes v56 back to disk and your working tree goes dirty again.

**Fix (do this before `git add`)**:

```bash
# 1. Find the active DB version for this cap
yesmem query --db yesmem --format objects \
  "SELECT id, content FROM learnings WHERE category='cap_definition' AND content LIKE '%\"name\":\"<name>\"%' ORDER BY id DESC LIMIT 1" \
| yesmem json '.[0].content | fromjson | .version'
# → e.g. 56

# 2. Bump the source version line to match
sed -i 's/^version: 55$/version: 56/' caps/bundled-caps/<name>/CAP.md

# 3. Re-verify SHA after the install step (deploy builds, migrate installs)
make deploy && yesmem migrate && sha256sum caps/bundled-caps/<name>/CAP.md ~/.claude/caps/<name>/CAP.md
```

Aligning **only** the body but not `version:` is the common slip — the body matches but the frontmatter line still drifts on every restart. The auto-supersede is silent; the only signal is the working-tree diff after the next daemon start.

**Alternative (cleaner)**: get the active version from the result of `activate_cap(<name>)` immediately after the `save_cap` call — the `code` field's registered tool registers under that exact version, and you can grep it back out before committing.

### Source vs deployed skill/cap path: source-tree wins

**Symptom**: A skill or cap edit in `skills/bundled-skills/<name>/SKILL.md` doesn't appear in the next session.

**Root cause**: There are two paths — the source-tree (`skills/bundled-skills/<name>/SKILL.md`) and the deployed cache (`~/.claude/skills/<name>/SKILL.md`). The latter is regenerated from the source on `make deploy` / install.

**Fix**: Always edit the source-tree file. Verify the deploy ran. Treat `~/.claude/...` as a deploy artifact, not as the source of truth.

---

## Bash-handler script gotchas

### `printf '- foo\n'` interprets leading `-` as option flag

**Symptom**: `printf '- foo\n'` fails with `bash: printf: -: invalid option`. Even with `--` separator, even quoted.

**Root cause**: Bash builtin printf interprets a leading `-` in the format string as an option flag. The `--` separator doesn't help (only works for some other builtins).

**Fix**:

```bash
printf '%s\n' '- foo'
```

**Stage-1**: [bb37bd60 msg:118, 296].

### `set -o pipefail` + `head -N` triggers SIGPIPE → block aborts silently

**Symptom**: A bash block with `set -o pipefail` runs `find | sort | head -10` and silently aborts. Files after the pipeline don't get created.

**Root cause**: `head -10` closes its stdin after 10 lines. Upstream `sort` gets SIGPIPE. With `pipefail`, the pipeline exits non-zero. With `set -e`, the block aborts.

**Fix**: Split via tempfile:

```bash
find ... | LC_ALL=C sort -r > "$WORK/list.txt" || true
head -n 10 "$WORK/list.txt" | while read -r f; do ... ; done
```

**Stage-1**: [bb37bd60 msg:130, 296], [cc0ba29d pinned-gotcha].

### `set -e` + missing dep silently kills the script

**Symptom**: `sqlite3: command not found` aborts the script with no useful error in the handler return. User has to dig into raw stderr.

**Root cause**: `set -e` kills on first non-zero exit. The dep-missing error goes to stderr; the handler captures stdout for JSON parsing and reports "parse failed".

**Fix**: Precheck deps at the top:

```bash
command -v sqlite3 jq base64 > /dev/null || { echo '{"ok":false,"error":"missing dep"}'; exit 1; }
```

**Stage-1**: [bb37bd60 msg:299].

### EXIT trap pattern for tempdir cleanup

**Pattern**:

```bash
WORK=$(mktemp -d -t mycap.XXXXXX)
trap 'rm -rf "$WORK"' EXIT
# ... use $WORK ...
```

Cleans up even on error/interrupt.

**Stage-1**: [bb37bd60 msg:73].

### /dev/shm/ for temp files in bash bodies

When a bash handler briefly needs a tempfile to parse (`head -c`, regex, jq), `/dev/shm/` is preferable to `/tmp/`:

- RAM-backed tmpfs — measurably faster
- No permission prompt — hooks don't gate `/dev/shm`
- Always `rm -f` in the same handler

```bash
TF=/dev/shm/mycap_$(date +%s%N)_$$.body
curl -sL --max-time 15 -o "$TF" "$URL"
head -c 262144 "$TF"
rm -f "$TF"
```

Name with `$(date +%s%N)_$$` so parallel handler invocations don't collide.

**Stage-1**: current skill "/dev/shm for fetch tempfiles".

### gojq reserves `label` as a keyword — function parameters with that name fail

**Symptom**: A jq function definition like `def kv(label; v): {(label): v};` aborts with `unexpected token "label"`. The error message points at the *call site*, not the definition, which makes it look like the calling expression is broken.

**Root cause**: gojq (used by `yesmem json` and the daemon's internal jq paths) reserves `label` as a control-flow keyword for `label $out | … | break $out`. It's reserved in **function-parameter position**, even though it parses fine as a field name (`.label`) or as the body of a `label $out` block.

**Fix**: Rename the parameter. Idiomatic substitutes: `lbl`, `name`, `key`, `tag`. Field accesses on `.label` are unaffected — only parameter declarations break.

**Note**: This is a gojq-specific limitation. Stock `jq` (libjq) accepts `label` as a parameter name. Caps that move between `yesmem json` and external `jq` need the rename to stay portable.

---

## Auto-correct loop

### autoCorrectBashCap on multi-script bundles — historical bug, now resolved

**Symptom (historical)**: A scheduled bash handler in a multi-script cap fails. Auto-correct generated a fix and accepted it. The cap's other scripts (the tool, the other handlers) were gone.

**Root cause (historical)**: `autoCorrectBashCap` in `internal/daemon/bash_error_handler.go` called `save_cap` with the legacy `handler_bash` field; `scriptsFromSaveCapParams` converted that to a single-script body, overwriting the entire `scripts` array. Multi-script bundles lost everything except the corrected handler.

**Resolution**: Fixed via script-targeted-update plumbing. `persistProposalForReview` now filters on `Scripts[i].Runtime == "bash" && Scripts[i].Name == run.ScriptName` and appends `script:<ScriptName>` as a keyword on the `cap_proposed` learning. `handleCapProposalDecide` extracts the script name from `proposal.Keywords` (prefix `script:`) — note: `LoadJunctionData(proposal)` must run **after** `GetLearning` (otherwise keywords come back empty) — and passes `scriptName` into `acceptCapProposal`, which replaces only the named script. Multi-script caps now benefit from auto-correct without bundle-wide destruction.

**Practical note**: This is the current behavior; older sessions' notes saying "set `auto_correct: false` on multi-script caps" are stale. If you see destructive behavior on a current build, that is a regression and worth a bug report — capture the proposal row's keywords field (should contain `script:<name>`).

**Stage-1 / supersede chain**: Bug originally captured at [cc0ba29d msg:87 "autoCorrectBashCap calls save_cap with legacy handler_bash param"]. Fix landed via the script-keyword-plumbing change to `persistProposalForReview` + `handleCapProposalDecide`.

### auto-correct flow — accept/reject staging

The daemon stages a Sonnet-fixed bash body as a `cap_proposed` learning. The active cap is untouched until you accept. Workflow:

```javascript
const pending = await mcp__yesmem__list_cap_proposals();
// inspect each: pending[i] has {id, cap_name, content (diff), original_error}
await mcp__yesmem__cap_proposal_decide({id: 12345, decision: 'accept'});
// or { decision: 'reject', notes: 'fix doesn't address the real bug' }
```

On accept: the proposed body is applied to the active cap via save_cap, proposal transitions to `cap_proposed_accepted`. On reject: proposal transitions to `cap_proposed_rejected`, active cap unchanged. Don't oversell — auto-correct is an assist, not magic. It produces a Sonnet draft you must review.

**Stage-1**: source: `internal/daemon/handler_cap_proposal.go`.

---

## Tool-result persistence

### Tool-result output >50 KB persisted under tool-results/

**Symptom**: A `get_caps` or other large tool returns "Output too large (54.9 KB)" with a path. The text isn't inline.

**Root cause**: The runtime auto-persists tool results >50 KB under `~/.claude/projects/<project>/<session>/tool-results/toolu_<id>.json` (or `.txt` for plain text) and only emits the path inline.

**Fix**: Read the persisted file directly. For JSON-array files: `jq -r '.[0].text' <file>`. For plain key:value REPL dumps: use `Read({file_path, offset, limit})` with line ranges (the file is NOT JSON; jq fails). Don't try to consume the whole thing as an inline string — context blows up.

**Caveat**: An REPL `o` object dumped to disk via the persistence mechanism is a `key:` followed by value lines; it's a text dump, not JSON. Use `grep -nE '^<key>:$'` to find section boundaries, then read by line offset.

**Stage-1**: hook gotcha "Tool-Result-Output >50KB wird vom Runtime automatisch unter tool-results/ persistiert".

### get_caps response too large for inline tool result

**Symptom**: `get_caps({name:'wiki_export'})` returns a 66 KB persistence path. The body is base64-embedded and pushes the row past the inline limit.

**Root cause**: A cap with a >10 KB embedded body (e.g. SCRIPT_B64 const) blows past the 50 KB tool-result wall.

**Fix**: Read the persisted file. For body inspection, slice by bytes: `python3 -c "print(open('/path/to/file').read()[A:B])"`.

**Stage-1**: [bb37bd60 msg:17-18].

---

## yesmem CLI for caps

### `yesmem query --format objects` vs `--format json`

**Symptom**: A cap that pipes `yesmem query | yesmem json` fails with `Cannot iterate over: object`.

**Root cause**: `yesmem query --format json` returns `{columns: [...], rows: [[...]]}` (matrix shape). jq filters expecting `.[].field` fail because the input is an object, not an array.

**Fix**: `yesmem query --format objects` returns array-of-objects (`[{...}, {...}]`, sqlite3-CLI-compatible). All `.[] | .field` filters work natively.

**Stage-1**: [cc0ba29d Workflow section], [bb37bd60 msg:665].

### `yesmem json` (gojq) does NOT support --slurpfile

**Symptom**: A cap ported from external jq fails with `unknown flag --slurpfile`.

**Root cause**: gojq parses stdin as a single JSON value via `json.Unmarshal`. There's no multi-input mode.

**Fix**: Collapse multi-dataset queries into one composite SQL with `json_group_array()` subselects on the SQL side, then `fromjson` on the jq side:

```sql
SELECT
  l.id, l.content,
  (SELECT json_group_array(value) FROM learning_entities WHERE learning_id=l.id) AS entities_json,
  (SELECT json_group_array(value) FROM learning_actions  WHERE learning_id=l.id) AS actions_json
FROM learnings l WHERE l.project = ?
```

```jq
((.entities_json // "[]") | fromjson) as $e |
((.actions_json  // "[]") | fromjson) as $a |
{ id, entities: $e, actions: $a }
```

**Stage-1**: [bb37bd60 msg:665, 785].

### Literal apostrophes inside `yesmem json '…'` break the bash quoting

**Symptom**: A bash-handler cap calls `yesmem json '… hybrid_search('\($q)') …'` and bash aborts with `Syntaxfehler beim unerwarteten Wort )` on the line containing the apostrophe, even though the jq filter looks correct. The error message points at the closing paren — far from the real cause.

**Root cause**: The outer bash single-quote ends at the first literal `'` *inside* the jq body. Everything after that is parsed as bash, including the `(`, `)`, and pipes — which is what triggers the misleading syntax error.

**Fix (preferred)**: Replace literal apostrophes inside the jq body with escaped double-quotes — gojq accepts both as string delimiters:

```bash
# break: literal apostrophe in the jq body
yesmem json '"hybrid_search('\''\($q)'\'')"'

# fix: use escaped double-quotes instead — robust, no bash-quote acrobatics
yesmem json "\"hybrid_search(\\\"\\($q)\\\")\""
# or (single-quoted bash, double-quoted jq strings):
yesmem json '"hybrid_search(\"\($q)\")"'
```

**Fix (mechanical)**: If you must keep literal apostrophes, use the bash close-escape-reopen idiom: `'\''` (close, escaped quote, reopen) for each one. Less readable, leaves bash-quote acrobatics in the jq source trail, easier to introduce a new bug while editing.

**Practical rule**: any cap whose jq emits identifiers it received as bash variables (search calls, CLI invocations with quoted args) is a candidate. Default to double-quoted jq strings; fall back to backticks for literal labels.

### `yesmem` CLI has no generic SQL read-path against yesmem.db

**Symptom**: `yesmem cap-store query learnings` fails — learnings is not a cap-namespaced table.

**Root cause**: cap_store is namespace-restricted to `cap_<name>__<table>`. There's no generic SQL passthrough through cap_store.

**Fix**: Use the new `yesmem query` subcommand (read-only SQL passthrough with driver-level guards: `mode=ro` URI + `PRAGMA query_only=1`). Use `yesmem json` for jq-style transforms.

**Stage-1**: [cc0ba29d Workflow section "yesmem CLI does NOT have a generic SQL read-path"].

---

## Spec feedback for cap-spec repo

These are gaps in the upstream cap-spec that stage-1 work surfaced. Candidates for follow-up PRs against `https://github.com/carsteneu/cap-spec`:

1. **Examples are still v1.0**. spec.md is at v1.1-draft, but `examples/reddit_fetch/CAP.md`, `examples/cap_search/CAP.md`, `examples/deploy/CAP.md`, and `examples/telegram/CAP.md` all use the v1.0 `## Script` (singular) cap-level-runtime format. v1.1 parser rejects them. Either migrate examples to v1.1 or revert spec.md to v1.0 — drift means README links lead to non-conformant files.

2. **No documented limit on `## Scripts` count**. Should there be a max-scripts-per-cap quota? `internal/capfile/parse.go` doesn't enforce one. Worth speccing for portability.

3. **`schema` derivation behavior is under-specified for REPL.** Spec says "derived from JS signature for tool-kind scripts in REPL runtime". The actual derivation rules (which destructured params are required vs optional, how defaults map to JSON Schema `default`, how rest params behave) aren't documented. Cap authors guess.

4. **Bundled-cap DB write-back behavior is not in the spec.** This is a YesMem-specific behavior, but the `cap_proposal_decide` flow assumes there is a DB. The spec should at minimum document that providers MAY persist caps separately from `~/.claude/caps/` and MAY write back on restart, and that authors must handle the source-vs-DB conflict.

5. **REPL VM allowlist not in `repl-prerequisites.md`.** That doc lists builtins (`sh`, `cat`, `put`, etc.) but doesn't enumerate what is forbidden (no `process`, no `require`, no `fetch`, no `file()` global unless adapter-prepended). A "forbidden" section would prevent the most common cap-write hallucinations.

6. **Cap-store quota constants aren't specced.** `CapsMaxTablesPerCap=10`, `CapsMaxRowsPerTable=10000`, `CapsMaxCellBytes=64*1024` are YesMem-side constants. The spec should either declare them as portability requirements or explicitly say providers may set their own.

7. **`sandbox` field is not in spec.md.** YesMem implements `sandbox: none|standard|strict` per-script with the `scope: project` + `sandbox: none` guard. The cap-spec metadata block doesn't list `sandbox` as a known key. Either spec it (with the guard rule) or move it to a yesmem-specific extension namespace.

8. **`scope` field semantics under-specified.** Spec says `user` vs `project`, but doesn't define behavior when scope is omitted. YesMem's parser defaults empty scope to "project" internally, which means the `sandbox=none` guard does NOT trigger on omitted-scope caps — counter-intuitive.

9. **`requires` auto-detection when handler doesn't use generic adapter names.** Spec says `requires` is auto-detected from `store(`/`web(`/`file(` syntax. Caps using `mcp__yesmem__cap_store(...)` directly (provider-specific, current YesMem reality after `ProviderToGeneric` runs) bypass detection. The auto-detection rule should clarify whether provider-specific names count.
