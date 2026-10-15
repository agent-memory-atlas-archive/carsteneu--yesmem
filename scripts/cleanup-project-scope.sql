-- ============================================================================
-- cleanup-project-scope.sql
--
-- Re-scope rows stored with project='' (global) that actually belong to a
-- specific project. A global row is injected into EVERY project's
-- pre-first-user-message context (briefing doc index, docs-hint, caps
-- catalog), so a mis-scoped row leaks across projects.
--
-- Target DB: ~/.claude/yesmem/yesmem.db  (only DB holding learnings/doc_sources)
-- Idempotent: every statement writes a fixed value, so a second run is a no-op.
-- Rows that already carry a non-empty project are never touched.
--
-- Rollback: scripts/cleanup-project-scope-rollback.sql
-- Wrapper:  scripts/cleanup-project-scope.sh  (backup + dry-run + apply + rollback)
--
-- ---------------------------------------------------------------------------
-- MAPPING TABLE — row -> proposed project -> evidence -> confidence
-- ---------------------------------------------------------------------------
--
-- caps (learnings.category='cap', superseded_by IS NULL):
--   | id    | cap                 | proposed project                                                       | evidence                                                                              | conf |
--   |-------|---------------------|------------------------------------------------------------------------|---------------------------------------------------------------------------------------|------|
--   | 65739 | gluten_shop_tools   | /home/chief/projects/gluten                                            | description "Bundled toolkit for gluten-shop research"; learnings project /home/chief/projects/gluten | high |
--   | 87262 | greencrawler_deploy | /var/www/html/GreenWashProjekt/greencrawler                            | description "Graceful Greencrawler-PHP deploy auf beide Produktionsworker"             | high |
--   | 91447 | gcm_log_audit       | /var/www/html/GreenWashProjekt/greenwashCCm19/cookie-consent-management | description "24h error/problem audit across GCM prod (gw-web1 …, greenwash.log …)"     | high |
--
-- doc_sources:
--   | id  | name      | proposed project                            | evidence                                                                 | conf |
--   |-----|-----------|---------------------------------------------|--------------------------------------------------------------------------|------|
--   | 122 | sulu-docs | /var/www/html/GreenWashProjekt/greenWebsite | path = /var/www/html/GreenWashProjekt/greenWebsite/docs/sulu-official     | high |
--   | 106 | rules     | /home/chief/memory/yesmem                   | path = /home/chief/memory/yesmem/CLAUDE.md, is_rules=1                    | high |
--   | 112 | go-effective | '' (global)                              | generic Go style reference, no project path                              | high |
--   | 113 | go-stdlib | '' (global)                                | generic Go stdlib reference, no project path                             | high |
--   | cinnamon-* (15, ids 87..101) | both cinnamon projects | see R3-3 below                                  | med  |
--
-- ---------------------------------------------------------------------------
-- R3-3 APPLIED — cinnamon-* are re-scoped to /home/chief/performance/cinnamon
-- and duplicated (doc_sources only) for /home/chief/projects/greenTile.
-- The round-2 review left two candidate targets: (a) /home/chief/remember
-- (literal ingest path of the skill files) and (b) /home/chief/performance/cinnamon
-- (the project the skills document). Chosen: the two cinnamon PROJECTS, because
-- the harm being fixed is the skills leaking into unrelated projects' context;
-- the ingest path is not a project string any session reports.
-- ---------------------------------------------------------------------------
-- ---------------------------------------------------------------------------
-- DELIBERATELY LEFT GLOBAL (generic / user-level, not project-scoped):
-- ---------------------------------------------------------------------------
--   doc_sources: superpowers:*, coding-style:*, frontend-design:*
--   caps:        wiki_export, rule_guard, broadcast_test, cap_save_analysis,
--                test_llm, generate_image, rocketchat, reddit, adapter_e2e_test,
--                cap_collect, cap_delete, cap_search, changelog_check,
--                proxy_health, chrome_headless, deploy, telegram
--
-- NOTE: cap opencode_patched_build is already scoped to a real project
-- (/home/chief/projects/opencode); it only leaked because the catalog was
-- unfiltered. The code fix resolves that — no data change needed.
--
-- RESOLVED (R4-5, user decision 2026-10-02): the stranded cap
-- learnings.id=71692 (reddit_reminder_today) is REMOVED rather than re-scoped —
-- its one-shot job is done. See the R4-5 block below. The other ~54 learnings
-- on the same dead project string are a separate class and stay untouched.
--
-- MATCHING: exact-equality (full path or basename) — never a suffix match, per
-- models.ProjectMatches doctrine (fuzzy matching collided on same-named repos).
-- Git-worktree sessions ARE covered: the caller project is canonicalized to the
-- main repo root via repo.RootOrSelf before matching (R3-2), so the exact
-- comparison then runs against the repo root these rows are scoped to.
-- ============================================================================

-- --- caps (learnings) -------------------------------------------------------
-- Writers may hold the DB (the daemon runs in WAL); wait rather than abort.
PRAGMA busy_timeout=15000;
BEGIN;
UPDATE learnings SET project='/home/chief/projects/gluten'
 WHERE id=65739 AND category='cap' AND superseded_by IS NULL AND (project='' OR project IS NULL);

UPDATE learnings SET project='/var/www/html/GreenWashProjekt/greencrawler'
 WHERE id=87262 AND category='cap' AND superseded_by IS NULL AND (project='' OR project IS NULL);

UPDATE learnings SET project='/var/www/html/GreenWashProjekt/greenwashCCm19/cookie-consent-management'
 WHERE id=91447 AND category='cap' AND superseded_by IS NULL AND (project='' OR project IS NULL);

-- --- doc_sources ------------------------------------------------------------
-- R3-1: rows stranded on the archived project string 'yesmem-projects-ghost'
-- (sessions under /home/chief/archiv/yesmem-projects-ghost, last 2026-08-18).
-- A dead project value makes them invisible in every project; two of them are
-- genuinely global Go references, one belongs to the yesmem repo itself.
UPDATE doc_sources SET project='/home/chief/memory/yesmem'
 WHERE id=106 AND name='rules' AND project='yesmem-projects-ghost';

UPDATE doc_sources SET project=''
 WHERE id=112 AND name='go-effective' AND project='yesmem-projects-ghost';

UPDATE doc_sources SET project=''
 WHERE id=113 AND name='go-stdlib' AND project='yesmem-projects-ghost';

UPDATE doc_sources SET project='/var/www/html/GreenWashProjekt/greenWebsite'
 WHERE id=122 AND name='sulu-docs' AND (project='' OR project IS NULL);

-- --- R3-3: cinnamon skill docs must be visible only in cinnamon projects -----
-- The 15 'cinnamon-*' doc sources were global (project=''), so every project's
-- briefing and plan docs hint advertised them. They belong to the two cinnamon
-- projects:
--   /home/chief/performance/cinnamon  (the Cinnamon desktop project)
--   /home/chief/projects/greenTile    (Cinnamon fork; its worktrees resolve to
--                                      this root via repo.RootOrSelf, see R3-2)
-- One doc_sources row holds exactly one project, so: re-scope the existing rows
-- to the first project, then add a duplicate row per source for the second.
--
-- These rows are is_skill=1 and carry their content in full_content — they have
-- ZERO rows in doc_chunks (verified: 0 chunks for every is_skill source). Their
-- doc_sources.chunk_count column is a stale advertised number, not a live count.
-- So the copy is a doc_sources row only; there are no chunks or FTS rows to
-- duplicate. The assertion below pins that assumption, so if a skill ever does
-- get chunked the script fails loudly instead of leaving a half-copied source.
--
-- Bounded to ids 87..101 (the verified set of the 15 sources), like every other
-- statement here, so the rollback inverts exactly what this moved. Drift in the
-- id set or row count is caught by the wrapper's per-group assertions.
--
-- Order matters: copy while the originals are still global, then re-scope them.
-- That way a greenTile copy exists only for a row this migration actually moved,
-- which is what lets the rollback be an exact inverse (it keys on "has a copy").
INSERT INTO doc_sources (name, version, path, url, project, chunk_count, is_skill, original_path, full_content, is_rules, trigger_extensions, doc_type, example_query, last_sync, created_at)
SELECT s.name, s.version, s.path, s.url, '/home/chief/projects/greenTile', s.chunk_count, s.is_skill, s.original_path, s.full_content, s.is_rules, s.trigger_extensions, s.doc_type, s.example_query, s.last_sync, s.created_at
  FROM doc_sources s
 WHERE s.id BETWEEN 87 AND 101 AND s.name LIKE 'cinnamon-%' AND (s.project='' OR s.project IS NULL)
   AND NOT EXISTS (SELECT 1 FROM doc_sources t WHERE t.name = s.name AND t.project='/home/chief/projects/greenTile');

UPDATE doc_sources SET project='/home/chief/performance/cinnamon'
 WHERE id BETWEEN 87 AND 101 AND name LIKE 'cinnamon-%' AND (project='' OR project IS NULL);

-- --- R4-5: remove the orphaned cap reddit_reminder_today (learnings.id=71692) --
-- User decision 2026-10-02: the cap can go. capProjectVisible() matches the exact
-- project string, so on the dead 'yesmem-projects-ghost' it is invisible to every
-- session; its one-shot Telegram reminder job is done.
-- Mirrors cap_delete's semantics for the learnings row + FK children. Note:
-- cap_delete writes `session_active_caps WHERE capability_name=...`, but the real
-- column is `cap_name`; this uses the correct name and has no row to delete.
-- Verified before writing: 0 knowledge_gaps.resolved_by rows, 0 session_active_caps
-- rows, no cap_reddit_reminder_today__* table, no cap_store_meta entry.
-- One gate for the whole block: if id 71692 is not exactly the expected cap row,
-- nothing is deleted — so a wrong id can never orphan children. Parent first, then
-- the FTS row, so a future FTS trigger on the parent delete cannot hit a row that
-- is already gone (which aborts the whole migration under -bail).
-- Idempotent: a second run finds no target row.
CREATE TEMP TABLE r45_target AS
  SELECT id FROM learnings
   WHERE id=71692 AND category='cap'
     AND COALESCE(json_extract(context,'$.cap_name'),'')='reddit_reminder_today';

DELETE FROM learnings                    WHERE id IN (SELECT id FROM r45_target);
DELETE FROM learnings_fts                WHERE rowid IN (SELECT id FROM r45_target);
DELETE FROM learning_entities            WHERE learning_id IN (SELECT id FROM r45_target);
DELETE FROM learning_actions             WHERE learning_id IN (SELECT id FROM r45_target);
DELETE FROM learning_keywords            WHERE learning_id IN (SELECT id FROM r45_target);
DELETE FROM learning_anticipated_queries WHERE learning_id IN (SELECT id FROM r45_target);
DELETE FROM anticipated_queries_fts      WHERE learning_id IN (SELECT id FROM r45_target);
DELETE FROM learning_cluster_scores      WHERE learning_id IN (SELECT id FROM r45_target);
DELETE FROM session_active_caps          WHERE cap_name='reddit_reminder_today' AND EXISTS (SELECT 1 FROM r45_target);

-- LIMITATION: when a session-cap store exists, activated caps live in caps.db
-- (storage/session_caps.go capDB()), not here — so this statement is a no-op for
-- the authoritative store. Verified 0 rows for this cap in both yesmem.db and
-- caps.db, so nothing is left dangling; a future "remove cap" migration must
-- target caps.db as well.

DROP TABLE r45_target;

-- --- verification -----------------------------------------------------------
COMMIT;
SELECT 'cinnamon_global_remaining' AS check_name, count(*) AS n
  FROM doc_sources WHERE name LIKE 'cinnamon-%' AND (project='' OR project IS NULL);

SELECT 'cinnamon_performance_rows' AS check_name, count(*) AS n
  FROM doc_sources WHERE id BETWEEN 87 AND 101 AND project='/home/chief/performance/cinnamon';

SELECT 'cinnamon_greentile_rows' AS check_name, count(*) AS n
  FROM doc_sources WHERE project='/home/chief/projects/greenTile'
   AND name IN (SELECT name FROM doc_sources WHERE id BETWEEN 87 AND 101);

-- Must stay 0: if a cinnamon skill ever gains chunks, the copy above has to be
-- extended to duplicate doc_chunks + doc_chunks_fts as well.
SELECT 'cinnamon_chunks_unexpected' AS check_name, count(*) AS n
  FROM doc_chunks c JOIN doc_sources s ON s.id = c.source_id WHERE s.name LIKE 'cinnamon-%';

SELECT 'r45_learning_removed' AS check_name, count(*) AS n
  FROM learnings WHERE id=71692;

SELECT 'r45_children_removed' AS check_name,
       (SELECT count(*) FROM learning_entities            WHERE learning_id=71692)
     + (SELECT count(*) FROM learning_actions             WHERE learning_id=71692)
     + (SELECT count(*) FROM learning_keywords            WHERE learning_id=71692)
     + (SELECT count(*) FROM learning_anticipated_queries WHERE learning_id=71692)
     + (SELECT count(*) FROM learning_cluster_scores      WHERE learning_id=71692)
     + (SELECT count(*) FROM anticipated_queries_fts      WHERE learning_id=71692)
     + (SELECT count(*) FROM learnings_fts                WHERE rowid=71692) AS n;

SELECT 'caps_targeted_still_global' AS check_name, count(*) AS n
  FROM learnings
 WHERE id IN (65739, 87262, 91447) AND (project='' OR project IS NULL);

SELECT 'sulu_docs_still_global' AS check_name, count(*) AS n
  FROM doc_sources WHERE id=122 AND (project='' OR project IS NULL);

SELECT 'ghost_rows_remaining' AS check_name, count(*) AS n
  FROM doc_sources WHERE id IN (106, 112, 113) AND project='yesmem-projects-ghost';
