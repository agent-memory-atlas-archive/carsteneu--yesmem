#!/usr/bin/env bash
# Project-scope data cleanup wrapper.
#
# Re-scopes rows stored with project='' that actually belong to a project
# (see scripts/cleanup-project-scope.sql for the mapping table + evidence).
#
#   ./cleanup-project-scope.sh dry-run   # default — print the rows that WOULD change
#   ./cleanup-project-scope.sh apply     # backup, then apply the migration
#   ./cleanup-project-scope.sh rollback  # backup, then reverse the migration
#
# DB: $YESMEM_DB, defaulting to ~/.claude/yesmem/yesmem.db
# Backup: <db>.bak-<label>-<UTC-timestamp> next to the DB (never overwritten).
#   The apply run labels its snapshot "pre-apply"; the rollback uses the newest
#   snapshot that still contains learnings.id=71692 to restore R4-5's deleted cap,
#   which has no SQL inverse.
#
# Never point this at a database you have not backed up. It writes only the
# rows listed in the mapping table; the backup is the guaranteed undo.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DB="${YESMEM_DB:-$HOME/.claude/yesmem/yesmem.db}"
MODE="${1:-dry-run}"

command -v sqlite3 >/dev/null || { echo "sqlite3 not found in PATH" >&2; exit 1; }
[ -f "$DB" ] || { echo "db not found: $DB" >&2; exit 1; }

backup() {
  local label="${1:-}" stamp b
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  b="${DB}.bak-${label:+${label}-}${stamp}-$$"
  # sqlite3 .backup is WAL-safe. A plain `cp` copies only the main db file, so
  # it can miss uncheckpointed -wal transactions or capture a torn image while
  # the daemon writes. The timeout covers a writer holding the DB.
  sqlite3 -cmd ".timeout 15000" "$DB" ".backup '$b'"
  echo "backup written: $b"
}

case "$MODE" in
  dry-run)
    echo "== rows that WOULD change in $DB =="
    sqlite3 -readonly "$DB" <<'SQL'
SELECT 'cap ' || id || ' ' || COALESCE(json_extract(context,'$.cap_name'),'?') ||
       ' -> $HOME/projects/gluten' AS change
  FROM learnings WHERE id=65739 AND category='cap' AND (project='' OR project IS NULL);
SELECT 'cap ' || id || ' ' || COALESCE(json_extract(context,'$.cap_name'),'?') ||
       ' -> /var/www/html/GreenWashProjekt/greencrawler' AS change
  FROM learnings WHERE id=87262 AND category='cap' AND (project='' OR project IS NULL);
SELECT 'cap ' || id || ' ' || COALESCE(json_extract(context,'$.cap_name'),'?') ||
       ' -> /var/www/html/GreenWashProjekt/greenwashCCm19/cookie-consent-management' AS change
  FROM learnings WHERE id=91447 AND category='cap' AND (project='' OR project IS NULL);
SELECT 'doc_source ' || id || ' ' || name || ' -> /var/www/html/GreenWashProjekt/greenWebsite' AS change
  FROM doc_sources WHERE id=122 AND (project='' OR project IS NULL);
SELECT 'doc_source ' || id || ' ' || name || ' -> $HOME/memory/yesmem' AS change
  FROM doc_sources WHERE id=106 AND project='yesmem-projects-ghost';
SELECT 'doc_source ' || id || ' ' || name || ' -> (global)' AS change
  FROM doc_sources WHERE id IN (112,113) AND project='yesmem-projects-ghost';
SELECT 'cinnamon-* (' || count(*) || ' rows) -> $HOME/performance/cinnamon' AS change
  FROM doc_sources WHERE name LIKE 'cinnamon-%' AND (project='' OR project IS NULL);
SELECT 'cinnamon-* copies to create -> $HOME/projects/greenTile' AS change
  WHERE NOT EXISTS (SELECT 1 FROM doc_sources WHERE name LIKE 'cinnamon-%' AND project='$HOME/projects/greenTile');
SELECT 'REMOVE cap ' || id || ' ' || COALESCE(json_extract(context,'$.cap_name'),'?') AS change
  FROM learnings WHERE id=71692;
SQL
    ;;
  apply)
    backup pre-apply
    sqlite3 -bail "$DB" < "$DIR/cleanup-project-scope.sql"
    # Positive, PER-GROUP assertion (counts separated by |). A single summed
    # total would let a compensating mismatch pass (e.g. 14 cinnamon + 16
    # greenTile still totals 30); per-group counts catch that, a wrong DB, and
    # renamed/missing ids — which a "still global?" check would silently pass.
    groups="$(sqlite3 "$DB" "SELECT
        (SELECT count(*) FROM learnings WHERE (id=65739 AND project='$HOME/projects/gluten')
            OR (id=87262 AND project='/var/www/html/GreenWashProjekt/greencrawler')
            OR (id=91447 AND project='/var/www/html/GreenWashProjekt/greenwashCCm19/cookie-consent-management')),
        (SELECT count(*) FROM doc_sources WHERE (id=122 AND project='/var/www/html/GreenWashProjekt/greenWebsite')
            OR (id=106 AND project='$HOME/memory/yesmem')
            OR (id=112 AND project='')
            OR (id=113 AND project='')),
        (SELECT count(*) FROM doc_sources WHERE id BETWEEN 87 AND 101 AND project='$HOME/performance/cinnamon'),
        (SELECT count(*) FROM doc_sources WHERE project='$HOME/projects/greenTile'
            AND name IN (SELECT name FROM doc_sources WHERE id BETWEEN 87 AND 101));")"
    if [ "$groups" != "3|4|15|15" ]; then
      echo "FAILED: expected per-group counts 3|4|15|15 (caps|doc_rows|cinnamon_perf|cinnamon_greentile), got $groups" >&2
      exit 1
    fi
    echo "applied: all 37 target rows re-scoped (per-group counts verified)."

    # The cinnamon copy is doc_sources-only because those rows are is_skill=1 with
    # zero chunks. If that ever changes the copy is incomplete, so fail loudly
    # rather than leave a source advertising chunks it does not have.
    chunks="$(sqlite3 "$DB" "SELECT count(*) FROM doc_chunks c JOIN doc_sources s ON s.id = c.source_id WHERE s.name LIKE 'cinnamon-%';" 2>/dev/null)"
    if [ "$chunks" != "0" ]; then
      echo "FAILED: cinnamon sources now have ${chunks:-<unknown>} chunks — extend cleanup-project-scope.sql to copy doc_chunks + doc_chunks_fts." >&2
      exit 1
    fi

    # R4-5 removed learnings.id=71692; assert the exact target row is gone, and
    # only then that its children are. Identity-gated like the DELETE itself, so a
    # DB where 71692 is a different learning does not report a false failure.
    tgt="$(sqlite3 "$DB" "SELECT count(*) FROM learnings WHERE id=71692 AND category='cap' AND COALESCE(json_extract(context,'\$.cap_name'),'')='reddit_reminder_today';")"
    if [ "$tgt" != "0" ]; then
      echo "FAILED: R4-5 target row still present (learnings.id=71692, cap reddit_reminder_today)." >&2
      exit 1
    fi
    kids="$(sqlite3 "$DB" "SELECT
        (SELECT count(*) FROM learning_entities            WHERE learning_id=71692)
      + (SELECT count(*) FROM learning_actions             WHERE learning_id=71692)
      + (SELECT count(*) FROM learning_keywords            WHERE learning_id=71692)
      + (SELECT count(*) FROM learning_anticipated_queries WHERE learning_id=71692)
      + (SELECT count(*) FROM learning_cluster_scores      WHERE learning_id=71692)
      + (SELECT count(*) FROM anticipated_queries_fts      WHERE learning_id=71692)
      + (SELECT count(*) FROM learnings_fts                WHERE rowid=71692);")"
    if [ "$kids" != "0" ]; then
      echo "FAILED: R4-5 row removed but $kids child rows remain." >&2
      exit 1
    fi
    echo "applied: R4-5 removed learnings.id=71692 (cap reddit_reminder_today)."
    ;;
  rollback)
    backup pre-rollback
    sqlite3 -bail "$DB" < "$DIR/cleanup-project-scope-rollback.sql"

    # R4-5 has no SQL inverse, so restore the deleted cap from a snapshot taken
    # before the deletion: the newest of this script's own pre-apply backups that
    # still contains the row (after a second `apply` the newest one no longer
    # does). Only this script's labelled backups are considered, and only ones
    # whose learnings table has the same column count, because the restore inserts
    # positionally.
    if [ "$(sqlite3 "$DB" "SELECT count(*) FROM learnings WHERE id=71692;")" != "0" ]; then
      echo "R4-5: learnings.id=71692 already present — nothing to restore."
    else
      want_cols="$(sqlite3 "$DB" "SELECT count(*) FROM pragma_table_info('learnings');")"
      # Any of this script's snapshots except a pre-rollback one (which is taken
      # after the delete and holds no copy). Identity + column-shape checked, so an
      # unrelated or older-schema backup cannot be used. mapfile keeps filenames
      # with spaces intact; a quote in the name is rejected because the path is
      # interpolated into a SQL string literal (the ATTACH below).
      src=""
      mapfile -t cands < <(ls -1t "$DB".bak-* 2>/dev/null)
      for cand in "${cands[@]}"; do
        [ -f "$cand" ] || continue
        case "$cand" in *"'"*|*-pre-rollback-*) continue ;; esac
        if [ "$(sqlite3 "$cand" "SELECT count(*) FROM pragma_table_info('learnings');" 2>/dev/null)" != "$want_cols" ]; then
          continue
        fi
        if [ "$(sqlite3 "$cand" "SELECT count(*) FROM learnings WHERE id=71692 AND category='cap' AND COALESCE(json_extract(context,'\$.cap_name'),'')='reddit_reminder_today';" 2>/dev/null)" = "1" ]; then
          src="$cand"
          break
        fi
      done
      restored=no
      if [ -n "$src" ]; then
        # Runs only when the parent is missing: most child tables have no unique
        # key (learning_cluster_scores is the exception), so a second rollback
        # would otherwise duplicate rows.
        if sqlite3 -bail -cmd ".timeout 15000" "$DB" "ATTACH '$src' AS b;
          BEGIN;
          INSERT INTO learnings SELECT * FROM b.learnings WHERE id=71692;
          INSERT INTO learning_entities            SELECT * FROM b.learning_entities            WHERE learning_id=71692;
          INSERT INTO learning_actions             SELECT * FROM b.learning_actions             WHERE learning_id=71692;
          INSERT INTO learning_keywords            SELECT * FROM b.learning_keywords            WHERE learning_id=71692;
          INSERT INTO learning_anticipated_queries SELECT * FROM b.learning_anticipated_queries WHERE learning_id=71692;
          INSERT INTO learning_cluster_scores      SELECT * FROM b.learning_cluster_scores      WHERE learning_id=71692;
          INSERT INTO learnings_fts (rowid, content) SELECT id, content FROM b.learnings WHERE id=71692;
          INSERT INTO anticipated_queries_fts (value, learning_id) SELECT value, learning_id FROM b.learning_anticipated_queries WHERE learning_id=71692;
          COMMIT;"; then
          restored=yes
          echo "restored: R4-5 learnings.id=71692 from $src"
        fi
      fi
      if [ "$restored" != "yes" ]; then
        # Fail closed: R4-5 is the only irreversible part of this migration, so a
        # missing restore must not be downgraded to a warning.
        echo "FAILED: R4-5 learnings.id=71692 (cap reddit_reminder_today) was NOT restored." >&2
        echo "        No usable backup found, or the restore failed. The rest of the rollback above succeeded." >&2
        echo "        Restore it manually from a backup taken before the apply run:" >&2
        echo "          sqlite3 '$DB' \"ATTACH '<backup>' AS b; INSERT INTO learnings SELECT * FROM b.learnings WHERE id=71692;\"" >&2
        echo "          (and the same for learning_entities/actions/keywords/anticipated_queries/cluster_scores + both FTS tables)" >&2
        exit 1
      fi
    fi
    back="$(sqlite3 "$DB" "SELECT
        (SELECT count(*) FROM learnings WHERE id IN (65739,87262,91447) AND (project='' OR project IS NULL)),
        (SELECT count(*) FROM doc_sources WHERE (id=122 AND (project='' OR project IS NULL))
            OR (id IN (106,112,113) AND project='yesmem-projects-ghost')),
        (SELECT count(*) FROM doc_sources WHERE id BETWEEN 87 AND 101 AND (project='' OR project IS NULL)),
        (SELECT count(*) FROM doc_sources WHERE project='$HOME/projects/greenTile'
            AND name IN (SELECT name FROM doc_sources WHERE id BETWEEN 87 AND 101));")"
    if [ "$back" != "3|4|15|0" ]; then
      echo "FAILED: expected per-group counts 3|4|15|0 after rollback, got $back" >&2
      exit 1
    fi
    echo "rolled back: 22 rows restored (per-group counts verified)."
    ;;
  *)
    echo "usage: $0 [dry-run|apply|rollback]" >&2
    exit 2
    ;;
esac
