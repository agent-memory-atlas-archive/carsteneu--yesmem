-- ============================================================================
-- cleanup-project-scope-rollback.sql
--
-- Reverse of scripts/cleanup-project-scope.sql. Restores project='' on exactly
-- the rows that migration re-scoped. Each statement is guarded on the value
-- the migration wrote, so a row that drifted since the migration is left alone
-- (the rollback never clobbers unrelated data).
--
-- Idempotent: a second run matches nothing.
-- Always take a backup first — scripts/cleanup-project-scope.sh rollback does.
-- ============================================================================

PRAGMA busy_timeout=15000;

BEGIN;

UPDATE learnings SET project=''
 WHERE id=65739 AND category='cap' AND project='/home/chief/projects/gluten';

UPDATE learnings SET project=''
 WHERE id=87262 AND category='cap' AND project='/var/www/html/GreenWashProjekt/greencrawler';

UPDATE learnings SET project=''
 WHERE id=91447 AND category='cap' AND project='/var/www/html/GreenWashProjekt/greenwashCCm19/cookie-consent-management';

UPDATE doc_sources SET project=''
 WHERE id=122 AND name='sulu-docs' AND project='/var/www/html/GreenWashProjekt/greenWebsite';

-- R3-1 rows: restore the original (archived) project value.
UPDATE doc_sources SET project='yesmem-projects-ghost'
 WHERE id=106 AND name='rules' AND project='/home/chief/memory/yesmem';

UPDATE doc_sources SET project='yesmem-projects-ghost'
 WHERE id=112 AND name='go-effective' AND project='';

UPDATE doc_sources SET project='yesmem-projects-ghost'
 WHERE id=113 AND name='go-stdlib' AND project='';

-- --- R3-3 rollback: re-globalize the moved originals, then drop the greenTile copies -
-- Guarded on the 15 cinnamon source ids (87..101, verified pre-migration) AND on a
-- greenTile copy existing: the apply only creates a copy for a row it moved, so
-- "has a copy" selects exactly the moved set (a cinnamon row already scoped to
-- the performance project has no copy and is left alone). Must run BEFORE the
-- delete, since the delete keys off the originals' names.
--
-- Residual (accepted): a cinnamon row that already existed under
-- /home/chief/projects/greenTile BEFORE the migration is indistinguishable from
-- one this script created, so the delete below removes it too. No such row
-- exists (verified pre-state: 0 greenTile cinnamon rows), and the wrapper's
-- per-group assertion aborts loudly if the counts ever drift.
UPDATE doc_sources SET project=''
 WHERE id BETWEEN 87 AND 101 AND name LIKE 'cinnamon-%' AND project='/home/chief/performance/cinnamon'
   AND name IN (SELECT name FROM doc_sources WHERE project='/home/chief/projects/greenTile');

DELETE FROM doc_sources
 WHERE project='/home/chief/projects/greenTile'
   AND name IN (SELECT name FROM doc_sources WHERE id BETWEEN 87 AND 101 AND name LIKE 'cinnamon-%');

COMMIT;

SELECT 'caps_restored_global' AS check_name, count(*) AS n
  FROM learnings
 WHERE id IN (65739, 87262, 91447) AND (project='' OR project IS NULL);

SELECT 'sulu_docs_restored_global' AS check_name, count(*) AS n
  FROM doc_sources WHERE id=122 AND (project='' OR project IS NULL);

SELECT 'ghost_rows_restored' AS check_name, count(*) AS n
  FROM doc_sources WHERE id IN (106, 112, 113) AND project='yesmem-projects-ghost';

SELECT 'cinnamon_reglobalized' AS check_name, count(*) AS n
  FROM doc_sources WHERE name LIKE 'cinnamon-%' AND (project='' OR project IS NULL);

SELECT 'cinnamon_greentile_remaining' AS check_name, count(*) AS n
  FROM doc_sources WHERE name LIKE 'cinnamon-%' AND project='/home/chief/projects/greenTile';

-- R4-5 has NO SQL inverse here: recreating learnings.id=71692 would mean ~55
-- columns plus its 4 keywords, 7 anticipated queries, 1 cluster score and the two
-- FTS rows, value by value. The wrapper restores it instead, from the labelled
-- pre-apply snapshot it records when you run `apply`:
--   cleanup-project-scope.sh rollback   (see the R4-5 restore step in the wrapper)
-- If you run this .sql file directly, restore id 71692 from any pre-apply
-- <db>.bak-pre-apply-* / <db>.bak-* file yourself.
SELECT 'r45_restore_via_wrapper' AS rollback_note, count(*) AS n FROM learnings WHERE id=71692;
