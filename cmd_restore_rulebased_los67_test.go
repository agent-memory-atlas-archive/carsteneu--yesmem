package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// The rule-based repair command knows two damaged classes: a row displaced
// against a real displacer, and a row a substance-free retire discarded with no
// successor at all (YM-467). The second one used to be a bare counter; it is
// listed now, and it needs its own apply — --apply must not silently pull it in.

type repairFixture struct {
	dbPath      string
	displacer   int64
	withReplace int64
	noSuccessor int64
	autoNoAssoc int64
}

func seedRepairFixture(t *testing.T) repairFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	dbPath := filepath.Join(dataDir, "yesmem.db")

	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}()

	insert := func(source, content string) int64 {
		t.Helper()
		id, err := store.InsertLearning(&models.Learning{
			Category:  "decision",
			Content:   content,
			Project:   "los67",
			Source:    source,
			CreatedAt: time.Now(),
			ModelUsed: "self",
		})
		if err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
		return id
	}
	displace := func(id, by int64, reason string) {
		t.Helper()
		if _, err := store.DB().Exec(
			`UPDATE learnings SET superseded_by = ?, supersede_reason = ?, valid_until = datetime('now') WHERE id = ?`,
			by, reason, id,
		); err != nil {
			t.Fatalf("displace %d: %v", id, err)
		}
	}

	f := repairFixture{dbPath: dbPath}
	f.displacer = insert("llm_extracted", "the near-duplicate that won")
	f.withReplace = insert("user_stated", "Pi-Bolt JSC: operand tables per codeblock")
	displace(f.withReplace, f.displacer, "rule-based: near-duplicate")
	f.noSuccessor = insert("user_stated", "Immer main")
	displace(f.noSuccessor, 0, "rule-based: substanzlos")
	f.autoNoAssoc = insert("llm_extracted", "Ja genau so")
	displace(f.autoNoAssoc, 0, "rule-based: substanzlos")
	return f
}

func runRestoreRuleBased(t *testing.T, args ...string) string {
	t.Helper()
	prevArgs, prevStderr := os.Args, os.Stderr
	t.Cleanup(func() { os.Args, os.Stderr = prevArgs, prevStderr })

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	os.Args = append([]string{"yesmem", "restore-rulebased-learnings"}, args...)
	os.Stderr = writer
	runRestoreRuleBasedLearnings()
	os.Stderr = prevStderr
	if err := writer.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out := <-done
	reader.Close()
	return out
}

func supersededBy(t *testing.T, dbPath string, id int64) *int64 {
	t.Helper()
	store, err := storage.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	l, err := store.GetLearning(id)
	if err != nil || l == nil {
		t.Fatalf("GetLearning(%d): %v", id, err)
	}
	return l.SupersededBy
}

// The dry run is the default and it is the only surface an operator decides on:
// both classes have to appear, each with its own count and its own apply switch,
// and nothing is written.
func TestRunRestoreRuleBased_DryRunListsBothClasses(t *testing.T) {
	f := seedRepairFixture(t)

	out := runRestoreRuleBased(t)

	for _, want := range []string{
		"1 learnings were displaced by the rule-based dedup",
		"1 user-owned learnings were retired as substance-free with no successor",
		"--apply-no-displacer",
		"--apply",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output does not name %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "no displacer") != 1 {
		t.Errorf("the no-successor row is not listed once:\n%s", out)
	}
	// The class does not filter on content shape, so the listing has to carry the
	// content: otherwise a tombstone marker reads like a statement and the
	// operator cannot decide.
	if !strings.Contains(out, "| Immer main") {
		t.Errorf("the no-successor listing does not show the content:\n%s", out)
	}
	for _, id := range []int64{f.withReplace, f.noSuccessor} {
		if !strings.Contains(out, "#"+itoa(id)) {
			t.Errorf("dry run output does not list #%d:\n%s", id, out)
		}
	}
	if !strings.Contains(out, "Nothing restored.") {
		t.Errorf("dry run claims to have restored something:\n%s", out)
	}
	if by := supersededBy(t, f.dbPath, f.noSuccessor); by == nil {
		t.Error("the dry run reactivated the no-successor row")
	}
	if by := supersededBy(t, f.dbPath, f.withReplace); by == nil {
		t.Error("the dry run reactivated the displacer victim")
	}
}

// --apply is the displacer class only. The no-successor row stays retired until
// its own switch is passed, and the run says so.
func TestRunRestoreRuleBased_ApplyLeavesTheNoSuccessorClassAlone(t *testing.T) {
	f := seedRepairFixture(t)

	out := runRestoreRuleBased(t, "--apply")

	if by := supersededBy(t, f.dbPath, f.withReplace); by != nil {
		t.Errorf("--apply left the displacer victim superseded by %d", *by)
	}
	if by := supersededBy(t, f.dbPath, f.noSuccessor); by == nil {
		t.Error("--apply reactivated the no-successor row without its own switch")
	}
	if !strings.Contains(out, "--apply-no-displacer") {
		t.Errorf("the run does not point at the remaining class:\n%s", out)
	}
}

// The second class needs its own explicit switch, and even then it takes only the
// user-owned rows: the machine-written twin stays retired.
func TestRunRestoreRuleBased_ApplyNoDisplacerClass(t *testing.T) {
	f := seedRepairFixture(t)

	out := runRestoreRuleBased(t, "--apply-no-displacer")

	if by := supersededBy(t, f.dbPath, f.noSuccessor); by != nil {
		t.Errorf("--apply-no-displacer left the user-owned row superseded by %d", *by)
	}
	if by := supersededBy(t, f.dbPath, f.autoNoAssoc); by == nil {
		t.Error("--apply-no-displacer reactivated a row the user does not own")
	}
	if by := supersededBy(t, f.dbPath, f.withReplace); by == nil {
		t.Error("--apply-no-displacer touched the displacer class")
	}
	if !strings.Contains(out, "Restored 1 of 1 learnings") {
		t.Errorf("the run does not report what it restored:\n%s", out)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
