package daemon

import (
	"path/filepath"
	"testing"
)

// The log dir creation, the run work dir and the sweep's live set each derive a
// directory element from a job. A row without an ID produced three different
// answers, so a live job's directory could read as an orphan and be removed.
// One key for all three closes that.
func TestLos57eJobKeyIsTheSameForEveryDirectoryHelper(t *testing.T) {
	h, _ := mustHandler(t)

	withID := &ScheduledJob{ID: "sched-los57e", Name: "los57e job"}
	if got := jobKey(withID.ID, withID.Name); got != "sched-los57e" {
		t.Errorf("jobKey(job with id) = %q, want the id", got)
	}

	withName := &ScheduledJob{Name: "los57e job"}
	if got := jobKey(withName.ID, withName.Name); got != "los57e job" {
		t.Errorf("jobKey(job without id) = %q, want the name", got)
	}

	// The log directory carries the same element the sweep protects.
	dir, err := h.ensureJobDir(withName)
	if err != nil {
		t.Fatalf("ensureJobDir: %v", err)
	}
	if got, want := filepath.Base(dir), jobDirName(jobKey(withName.ID, withName.Name)); got != want {
		t.Errorf("ensureJobDir named the dir %q, the sweep protects %q", got, want)
	}
	if got, want := filepath.Base(dir), jobDirName(withName.Name); got != want {
		t.Errorf("ensureJobDir named the dir %q, want the name-keyed %q", got, want)
	}

	// The run work dir belongs to that same key.
	work, err := EnsureRunWorkDir(h.dataDir, jobKey(withName.ID, withName.Name))
	if err != nil {
		t.Fatalf("EnsureRunWorkDir: %v", err)
	}
	if got, want := filepath.Base(work), jobDirName(jobKey(withName.ID, withName.Name)); got != want {
		t.Errorf("run work dir %q, want the key-keyed %q", got, want)
	}
}
