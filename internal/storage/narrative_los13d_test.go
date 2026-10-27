package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// los13dSeedSession writes a session row plus its messages. textMsgs become
// message_type=text with content, otherMsgs become tool_use rows without
// content, so a test can state both the total count and the narratable part.
func los13dSeedSession(t *testing.T, s *Store, id, project string, textMsgs, otherMsgs int) models.Session {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	sess := models.Session{
		ID: id, Project: project,
		MessageCount: textMsgs + otherMsgs,
		StartedAt:    now.Add(-time.Hour), EndedAt: now,
		JSONLPath: "/" + id + ".jsonl", IndexedAt: now,
	}
	if err := s.UpsertSession(&sess); err != nil {
		t.Fatalf("upsert session %s: %v", id, err)
	}
	msgs := make([]models.Message, 0, textMsgs+otherMsgs)
	for i := 0; i < textMsgs; i++ {
		msgs = append(msgs, models.Message{
			SessionID: id, Role: "user", MessageType: "text",
			Content: fmt.Sprintf("text message %d", i), Timestamp: now, Sequence: i,
		})
	}
	for i := 0; i < otherMsgs; i++ {
		msgs = append(msgs, models.Message{
			SessionID: id, Role: "assistant", MessageType: "tool_use",
			ToolName: "bash", Timestamp: now, Sequence: textMsgs + i,
		})
	}
	if len(msgs) > 0 {
		if err := s.InsertMessages(msgs); err != nil {
			t.Fatalf("insert messages for %s: %v", id, err)
		}
	}
	return sess
}

// los13dInsertNarrative writes a narrative learning for a session.
func los13dInsertNarrative(t *testing.T, s *Store, sessionID, project, content string, createdAt time.Time) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		Category: "narrative", Content: content, Project: project, SessionID: sessionID,
		CreatedAt: createdAt, ModelUsed: "test", Confidence: 1.0, Source: "llm_extracted",
	})
	if err != nil {
		t.Fatalf("insert narrative: %v", err)
	}
	return id
}

// The callers pass sessions.project_short, an absolute path; narrative rows carry
// canonical_project, the basename. Comparing the path against canonical_project
// matched nothing, so every generated narrative stayed in the table (524 rows for
// one project on the live DB).
func TestDeleteOldNarratives_ReachesTheCallersProject(t *testing.T) {
	s := mustOpen(t)
	project := "/home/tester/memory/yesmem"
	base := time.Now().Truncate(time.Second).Add(-4 * time.Hour)
	for i := 0; i < 4; i++ {
		los13dInsertNarrative(t, s, fmt.Sprintf("sess-%d", i), project, fmt.Sprintf("story %d", i), base.Add(time.Duration(i)*time.Minute))
	}

	deleted, err := s.DeleteOldNarratives(project, 2, 0)
	if err != nil {
		t.Fatalf("delete old narratives: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted %d rows, want 2 (the project path has to reach canonical_project)", deleted)
	}

	rows, err := s.DB().Query(`SELECT content FROM learnings WHERE category = 'narrative' ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("read survivors: %v", err)
	}
	defer rows.Close()
	var survivors []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatalf("scan survivor: %v", err)
		}
		survivors = append(survivors, content)
	}
	want := []string{"story 2", "story 3"}
	if len(survivors) != len(want) || survivors[0] != want[0] || survivors[1] != want[1] {
		t.Fatalf("survivors = %v, want the two newest %v", survivors, want)
	}
}

// narrative_at is a marker, not evidence: after a cleanup or a manual delete the
// marker can outlive the row. The coverage number must count rows that actually
// exist and are recallable.
func TestCoverageStats_NarrativeDoneCountsActiveRows(t *testing.T) {
	s := mustOpen(t)
	project := "/home/tester/memory/yesmem"
	now := time.Now().Truncate(time.Second)

	gone := los13dSeedSession(t, s, "sess-gone", project, 6, 0)
	if err := s.MarkSessionNarrative(gone.ID); err != nil {
		t.Fatalf("mark narrative: %v", err)
	}
	goneNarrative := los13dInsertNarrative(t, s, gone.ID, project, "superseded story", now)
	if err := s.SupersedeByIDs([]int64{goneNarrative}, -1, "test cleanup"); err != nil {
		t.Fatalf("supersede narrative: %v", err)
	}

	live := los13dSeedSession(t, s, "sess-live", project, 6, 0)
	if err := s.MarkSessionNarrative(live.ID); err != nil {
		t.Fatalf("mark narrative: %v", err)
	}
	los13dInsertNarrative(t, s, live.ID, project, "active story", now)

	cs, err := s.GetCoverageStats()
	if err != nil {
		t.Fatalf("coverage stats: %v", err)
	}
	if cs.NarrativeDone != 1 {
		t.Errorf("NarrativeDone = %d, want 1 (only a session with an active narrative row counts)", cs.NarrativeDone)
	}
	// The session whose narrative was pruned keeps its marker on purpose; it is
	// not part of the workload any more, so it must not sit in the denominator.
	if cs.NarrativeTotal != 1 {
		t.Errorf("NarrativeTotal = %d, want 1 (open plus satisfied, not every narratable session)", cs.NarrativeTotal)
	}
}

// The narrative rows of the coverage block ignored the StatsFilter, so a
// project-scoped report showed another project's narrative backlog.
func TestCoverageStats_NarrativeHonoursFilter(t *testing.T) {
	s := mustOpen(t)
	los13dSeedSession(t, s, "sess-alpha", "/work/alpha", 6, 0)
	los13dSeedSession(t, s, "sess-beta", "/work/beta", 6, 0)

	cs, err := s.GetCoverageStatsF(StatsFilter{Project: "/work/alpha"})
	if err != nil {
		t.Fatalf("coverage stats: %v", err)
	}
	if cs.NarrativeTotal != 1 {
		t.Errorf("NarrativeTotal = %d, want 1 for the filtered project", cs.NarrativeTotal)
	}
	if cs.NarrativeDone != 0 {
		t.Errorf("NarrativeDone = %d, want 0 for the filtered project", cs.NarrativeDone)
	}
}

// The cleanup collected the target ids on the reader handle and wrote on the
// writer handle, so the returned id set could describe a different row set than
// the update touched. A read handle that is unavailable must not break the
// cleanup: the ids have to come from the same statement that writes.
func TestSupersedeShortNarratives_SurvivesWithoutReadHandle(t *testing.T) {
	s := mustOpen(t)

	now := time.Now().Truncate(time.Second)
	sess := models.Session{
		ID: "sess-short", Project: "/work/short", MessageCount: 2,
		StartedAt: now.Add(-72 * time.Hour), EndedAt: now.Add(-48 * time.Hour),
		JSONLPath: "/sess-short.jsonl", IndexedAt: now,
	}
	if err := s.UpsertSession(&sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := s.MarkSessionNarrative(sess.ID); err != nil {
		t.Fatalf("mark narrative: %v", err)
	}
	narrativeID := los13dInsertNarrative(t, s, sess.ID, "/work/short", "short story", now)

	closed, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open stand-in read handle: %v", err)
	}
	if err := closed.Close(); err != nil {
		t.Fatalf("close stand-in read handle: %v", err)
	}
	s.readDB = closed

	ids, err := s.SupersedeShortNarratives()
	if err != nil {
		t.Fatalf("cleanup must not depend on a separate read handle: %v", err)
	}
	if len(ids) != 1 || ids[0] != narrativeID {
		t.Fatalf("superseded ids = %v, want [%d]", ids, narrativeID)
	}

	var marker sql.NullString
	if err := s.DB().QueryRow(`SELECT narrative_at FROM sessions WHERE id = ?`, sess.ID).Scan(&marker); err != nil {
		t.Fatalf("read narrative_at: %v", err)
	}
	if marker.Valid {
		t.Error("a session whose narrative the cleanup removed must lose its narrative_at marker")
	}
}

// A narrative carries its session's start time, so the row written for an old
// session sorts to the bottom of its project. The prune must not delete the row
// the caller just wrote: the caller marks that session as narrated right after,
// and a marker without a narrative keeps the session unnarrated for good.
func TestDeleteOldNarratives_KeepsTheRowJustWritten(t *testing.T) {
	s := mustOpen(t)
	project := "/home/tester/memory/yesmem"
	base := time.Now().Truncate(time.Second).Add(-4 * time.Hour)

	// An old session: its narrative is written now but carries the old start time.
	spare := los13dInsertNarrative(t, s, "sess-old", project, "story old", base.Add(-2*time.Hour))
	for i := 0; i < 3; i++ {
		los13dInsertNarrative(t, s, fmt.Sprintf("sess-%d", i), project, fmt.Sprintf("story %d", i), base.Add(time.Duration(i)*time.Minute))
	}

	deleted, err := s.DeleteOldNarratives(project, 2, spare)
	if err != nil {
		t.Fatalf("delete old narratives: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d rows, want 1 (the keep set plus the spare row stay)", deleted)
	}
	var survived int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE id = ?`, spare).Scan(&survived); err != nil {
		t.Fatalf("read spare row: %v", err)
	}
	if survived != 1 {
		t.Error("the row the caller just wrote must survive the prune")
	}
}

// A row written more recently than the keep-newest ones survives as well: an
// inline caller prunes right after its insert, so the newest write must not be the
// first victim just because it describes an old session.
func TestDeleteOldNarratives_SparesTheRowWrittenLast(t *testing.T) {
	s := mustOpen(t)
	project := "/home/tester/memory/yesmem"
	base := time.Now().Truncate(time.Second).Add(-4 * time.Hour)

	los13dInsertNarrative(t, s, "sess-newest", project, "newest story", base.Add(2*time.Minute))
	los13dInsertNarrative(t, s, "sess-middle", project, "middle story", base.Add(time.Minute))
	los13dInsertNarrative(t, s, "sess-oldest", project, "oldest story", base)

	deleted, err := s.DeleteOldNarratives(project, 2, 0)
	if err != nil {
		t.Fatalf("delete old narratives: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted %d rows, want 0: the keep set by insertion order covers the oldest story", deleted)
	}
}

// A basename is not a delete scope: two repositories named alike share the
// canonical_project value, and a narrative row of the other one must survive.
func TestDeleteOldNarratives_StaysInsideTheRepository(t *testing.T) {
	s := mustOpen(t)
	base := time.Now().Truncate(time.Second).Add(-4 * time.Hour)
	for i := 0; i < 3; i++ {
		los13dInsertNarrative(t, s, fmt.Sprintf("own-%d", i), "/work/a/yesmem", fmt.Sprintf("own %d", i), base.Add(time.Duration(i)*time.Minute))
	}
	// Same basename, different repository, oldest session time: a canonical
	// projection would rank it out and delete it.
	foreign := los13dInsertNarrative(t, s, "foreign", "/work/b/yesmem", "foreign story", base.Add(-time.Hour))

	deleted, err := s.DeleteOldNarratives("/work/a/yesmem", 2, 0)
	if err != nil {
		t.Fatalf("delete old narratives: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d rows, want 1 (only the repository's own oldest story)", deleted)
	}
	var survived int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE id = ?`, foreign).Scan(&survived); err != nil {
		t.Fatalf("read foreign row: %v", err)
	}
	if survived != 1 {
		t.Error("a repository named like another must keep its narratives")
	}
}

// A worktree belongs to its repository: a prune started from a worktree keeps the
// repository's narratives in one scope, exactly like the briefing reads them.
func TestDeleteOldNarratives_FoldsWorktreesIntoTheirRepository(t *testing.T) {
	s := mustOpen(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("create git dir: %v", err)
	}
	base := time.Now().Truncate(time.Second).Add(-4 * time.Hour)
	mainRow := los13dInsertNarrative(t, s, "main-session", repo, "main story", base.Add(-time.Hour))
	los13dInsertNarrative(t, s, "wt-1", filepath.Join(repo, ".worktrees", "one"), "worktree story 1", base.Add(time.Minute))
	los13dInsertNarrative(t, s, "wt-2", filepath.Join(repo, ".worktrees", "two"), "worktree story 2", base.Add(2*time.Minute))
	los13dInsertNarrative(t, s, "wt-3", filepath.Join(repo, ".worktrees", "three"), "worktree story 3", base.Add(3*time.Minute))

	deleted, err := s.DeleteOldNarratives(filepath.Join(repo, ".worktrees", "one"), 2, 0)
	if err != nil {
		t.Fatalf("delete old narratives: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d rows, want 2 from the shared repository scope", deleted)
	}
	var survived int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE id = ?`, mainRow).Scan(&survived); err != nil {
		t.Fatalf("read main row: %v", err)
	}
	if survived != 0 {
		t.Error("a worktree prune must see the repository's narratives, not only its own")
	}
}

// A repository path can carry non-ASCII characters. substr counts characters
// while len() counts bytes, so a byte length in the prefix test would cut the
// path too late and every descendant would fall out of the scope.
func TestDeleteOldNarratives_HandlesNonASCIIPaths(t *testing.T) {
	s := mustOpen(t)
	repo := filepath.Join(t.TempDir(), "Müller", "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("create git dir: %v", err)
	}
	base := time.Now().Truncate(time.Second).Add(-4 * time.Hour)
	for i := 0; i < 3; i++ {
		los13dInsertNarrative(t, s, fmt.Sprintf("own-%d", i), filepath.Join(repo, "sub"), fmt.Sprintf("own %d", i), base.Add(time.Duration(i)*time.Minute))
	}

	deleted, err := s.DeleteOldNarratives(repo, 2, 0)
	if err != nil {
		t.Fatalf("delete old narratives: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d rows, want 1 from a repository whose path has non-ASCII characters", deleted)
	}
}

// A plain parent directory is not a repository. Scoping its narratives to
// everything below it would let one prune sweep every project under the parent:
// on the live DB a prune named "/home/tester" reached 1802 rows across ~350
// projects.
func TestDeleteOldNarratives_DoesNotSweepAParentDirectory(t *testing.T) {
	s := mustOpen(t)
	parent := t.TempDir()
	nested := filepath.Join(parent, "other")
	base := time.Now().Truncate(time.Second).Add(-4 * time.Hour)

	for i := 0; i < 3; i++ {
		los13dInsertNarrative(t, s, fmt.Sprintf("own-%d", i), parent, fmt.Sprintf("own %d", i), base.Add(time.Duration(i)*time.Minute))
	}
	var nestedIDs []int64
	for i := 0; i < 5; i++ {
		nestedIDs = append(nestedIDs, los13dInsertNarrative(t, s, fmt.Sprintf("nested-%d", i), nested, fmt.Sprintf("nested %d", i), base.Add(time.Duration(i)*time.Minute)))
	}

	deleted, err := s.DeleteOldNarratives(parent, 2, 0)
	if err != nil {
		t.Fatalf("delete old narratives: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d rows, want 1: a non-repository parent must not reach the projects below it", deleted)
	}
	for _, id := range nestedIDs {
		var survived int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE id = ?`, id).Scan(&survived); err != nil {
			t.Fatalf("read nested row %d: %v", id, err)
		}
		if survived != 1 {
			t.Fatalf("nested narrative %d was deleted by a prune of its parent directory", id)
		}
	}
}

// The projection names one project; the prune must not reach into another one.
func TestDeleteOldNarratives_LeavesForeignProjectsAlone(t *testing.T) {
	s := mustOpen(t)
	base := time.Now().Truncate(time.Second).Add(-4 * time.Hour)
	for i := 0; i < 3; i++ {
		los13dInsertNarrative(t, s, fmt.Sprintf("own-%d", i), "/work/own", fmt.Sprintf("own %d", i), base.Add(time.Duration(i)*time.Minute))
	}
	foreign := los13dInsertNarrative(t, s, "foreign", "/work/foreign", "foreign story", base)

	if _, err := s.DeleteOldNarratives("/work/own", 2, 0); err != nil {
		t.Fatalf("delete old narratives: %v", err)
	}
	var survived int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE id = ?`, foreign).Scan(&survived); err != nil {
		t.Fatalf("read foreign row: %v", err)
	}
	if survived != 1 {
		t.Error("a prune of one project must not touch another project's narratives")
	}
}

// keep=0 would delete every narrative of the project. Now that the prune actually
// runs, that has to be an error rather than a silent mass delete.
func TestDeleteOldNarratives_RejectsKeepBelowOne(t *testing.T) {
	s := mustOpen(t)
	if _, err := s.DeleteOldNarratives("/work/anything", 0, 0); err == nil {
		t.Fatal("keep=0 must be rejected, not delete every narrative of the project")
	}
}

// The spared ids go through batches because their count is caller data and
// SQLite's variable limit is a hard wall.
func TestChunkInt64_BatchesWithoutLosingIds(t *testing.T) {
	ids := make([]int64, 0, 1201)
	for i := int64(1); i <= 1201; i++ {
		ids = append(ids, i)
	}
	var got []int64
	for _, batch := range chunkInt64(ids, 500) {
		got = append(got, batch...)
	}
	if len(got) != len(ids) {
		t.Fatalf("batches hold %d ids, want %d", len(got), len(ids))
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("id %d = %d, want %d", i, got[i], ids[i])
		}
	}
}

// A quarantined session must stay out of the coverage total: it can never be
// narrated, so counting it as missing reports a gap no run can close.
func TestCoverageStats_NarrativeExcludesQuarantinedSessions(t *testing.T) {
	s := mustOpen(t)
	project := "/work/quarantined"
	open := los13dSeedSession(t, s, "sess-open", project, 6, 0)
	quarantined := los13dSeedSession(t, s, "sess-quarantined", project, 6, 0)
	if open.ID == quarantined.ID {
		t.Fatal("test sessions must be distinct")
	}
	if _, err := s.InsertLearning(&models.Learning{
		Category: "pattern", Content: "unreliable claim of a crashed agent",
		Project: project, SessionID: quarantined.ID,
		CreatedAt: time.Now().Truncate(time.Second), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}
	if _, err := s.QuarantineSession(quarantined.ID); err != nil {
		t.Fatalf("quarantine session: %v", err)
	}

	cs, err := s.GetCoverageStats()
	if err != nil {
		t.Fatalf("coverage stats: %v", err)
	}
	if cs.NarrativeTotal != 1 {
		t.Errorf("NarrativeTotal = %d, want 1 (the quarantined session is not narratable)", cs.NarrativeTotal)
	}
}
