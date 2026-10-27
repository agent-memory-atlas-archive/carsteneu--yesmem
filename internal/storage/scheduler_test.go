package storage

import (
	"testing"
	"time"
)

func TestScheduledJobCapName(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	job := ScheduledJobRow{
		ID:          "test-bash-1",
		Name:        "test-cap-runner",
		Cron:        "*/5 * * * *",
		Mode:        "bash",
		CapName:     "proxy_health",
		AutoCorrect: true,
		Enabled:     true,
		Recurring:   true,
	}
	if err := s.SaveScheduledJob(job); err != nil {
		t.Fatalf("save: %v", err)
	}

	jobs, err := s.ListScheduledJobs()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, j := range jobs {
		if j.ID == "test-bash-1" {
			found = true
			if j.CapName != "proxy_health" {
				t.Errorf("cap_name = %q, want %q", j.CapName, "proxy_health")
			}
			if j.Mode != "bash" {
				t.Errorf("mode = %q, want %q", j.Mode, "bash")
			}
			if !j.AutoCorrect {
				t.Error("auto_correct should be true")
			}
		}
	}
	if !found {
		t.Error("job not found after save")
	}
}

func TestBashJobRunStorage(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	run := BashJobRun{
		JobID:    "test-job-1",
		JobName:  "broken-script",
		CapName:  "test_cap",
		Command:  "exit 1",
		Status:   "error",
		ExitCode: 1,
		Output:   "some output",
		ErrorMsg: "exit status 1",
	}
	if err := s.SaveBashJobRun(run); err != nil {
		t.Fatalf("save: %v", err)
	}

	runs, err := s.GetBashJobRuns("test-job-1", 10)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].Status != "error" || runs[0].ExitCode != 1 {
		t.Errorf("unexpected run: %+v", runs[0])
	}
	if runs[0].Processed {
		t.Error("new run should not be processed")
	}

	errors, err := s.GetUnprocessedBashErrors(10)
	if err != nil {
		t.Fatalf("get errors: %v", err)
	}
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}

	if err := s.MarkBashJobRunProcessed(errors[0].ID); err != nil {
		t.Fatalf("mark: %v", err)
	}

	errors2, _ := s.GetUnprocessedBashErrors(10)
	if len(errors2) != 0 {
		t.Errorf("expected 0 unprocessed, got %d", len(errors2))
	}
}

func TestPurgeBashJobRunsRetention(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	now := time.Now()
	runs := []struct {
		name      string
		status    string
		processed bool
		createdAt time.Time
	}{
		{name: "old success", status: "ok", createdAt: now.Add(-25 * time.Hour)},
		{name: "boundary success", status: "ok", createdAt: now.Add(-24 * time.Hour)},
		{name: "recent success", status: "ok", createdAt: now.Add(-23 * time.Hour)},
		{name: "old processed error", status: "error", processed: true, createdAt: now.Add(-31 * 24 * time.Hour)},
		{name: "boundary processed error", status: "error", processed: true, createdAt: now.Add(-30 * 24 * time.Hour)},
		{name: "recent processed error", status: "error", processed: true, createdAt: now.Add(-29 * 24 * time.Hour)},
		{name: "old pending error", status: "error", createdAt: now.Add(-90 * 24 * time.Hour)},
	}
	for _, run := range runs {
		result, err := s.DB().Exec(`INSERT INTO bash_job_runs
			(job_id, job_name, command, status, processed, created_at)
			VALUES (?, ?, '', ?, ?, ?)`, run.name, run.name, run.status, run.processed,
			run.createdAt.UTC().Format(sqliteDateTimeFormat))
		if err != nil {
			t.Fatalf("insert %s: %v", run.name, err)
		}
		if _, err := result.LastInsertId(); err != nil {
			t.Fatalf("insert id %s: %v", run.name, err)
		}
	}

	deleted, err := s.PurgeBashJobRuns(now.Add(-24*time.Hour), now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}

	rows, err := s.DB().Query(`SELECT job_name FROM bash_job_runs ORDER BY job_name`)
	if err != nil {
		t.Fatalf("query remaining: %v", err)
	}
	defer rows.Close()
	remaining := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan remaining: %v", err)
		}
		remaining[name] = true
	}
	for _, want := range []string{
		"boundary success", "boundary processed error",
		"recent success", "recent processed error",
		"old pending error",
	} {
		if !remaining[want] {
			t.Errorf("expected %q to remain", want)
		}
	}
}

// YM-170: last_run went into SQLite as the driver's own representation of a
// time.Time and came back unparseable, because ListScheduledJobs reads RFC3339.
// The value silently ended up as the zero time, so a reloaded job lost its last
// run and any "when did this fire" answer was wrong.
func TestUpdateJobLastRun_IsReadableAfterAReload(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	job := ScheduledJobRow{
		ID: "test-last-run", Name: "last-run", Cron: "* * * * *",
		Mode: "bash", Enabled: true, Recurring: true,
	}
	if err := s.SaveScheduledJob(job); err != nil {
		t.Fatalf("save: %v", err)
	}

	when := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	if err := s.UpdateJobLastRun(job.ID, when); err != nil {
		t.Fatalf("update last run: %v", err)
	}

	var raw string
	if err := s.DB().QueryRow(`SELECT last_run FROM scheduled_jobs WHERE id = ?`, job.ID).Scan(&raw); err != nil {
		t.Fatalf("read raw last_run: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		t.Fatalf("stored last_run %q is not RFC3339: %v", raw, err)
	}

	jobs, err := s.ListScheduledJobs()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, j := range jobs {
		if j.ID != job.ID {
			continue
		}
		found = true
		if j.LastRun.IsZero() {
			t.Fatalf("last_run did not survive the reload (stored %q)", raw)
		}
		if !j.LastRun.Equal(when) {
			t.Errorf("last_run = %v, want %v", j.LastRun, when)
		}
	}
	if !found {
		t.Fatal("job not found after save")
	}
}

// A job that never ran must not carry a fabricated timestamp: the zero time was
// written verbatim, and after a restart it read back as year 1.
func TestSaveScheduledJob_ZeroLastRunStaysNull(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	job := ScheduledJobRow{
		ID: "test-never-ran", Name: "never-ran", Cron: "* * * * *",
		Mode: "bash", Enabled: true, Recurring: true,
	}
	if err := s.SaveScheduledJob(job); err != nil {
		t.Fatalf("save: %v", err)
	}

	var isNull bool
	if err := s.DB().QueryRow(`SELECT last_run IS NULL FROM scheduled_jobs WHERE id = ?`, job.ID).Scan(&isNull); err != nil {
		t.Fatalf("read last_run: %v", err)
	}
	if !isNull {
		t.Fatal("a job that never ran must store last_run as NULL, not as the zero time")
	}

	jobs, err := s.ListScheduledJobs()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, j := range jobs {
		if j.ID == job.ID && !j.LastRun.IsZero() {
			t.Errorf("last_run = %v, want zero", j.LastRun)
		}
	}
}

// A one-shot that fired before YM-169 is still in the table and would fire a
// second time on the next start. A one-shot that never ran must survive: the old
// write path gave it the zero time as last_run.
func TestDeleteFiredOneShotJobs_KeepsPendingOnes(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	rows := []struct {
		id      string
		recur   int
		lastRun string
	}{
		{"fired", 0, "2026-10-17 09:30:00 +0000 UTC"},
		{"pending", 0, "0001-01-01 00:00:00 +0000 UTC"},
		{"repeat", 1, "2026-10-17 09:30:00 +0000 UTC"},
	}
	for _, r := range rows {
		if _, err := s.DB().Exec(`INSERT INTO scheduled_jobs (id, name, cron, prompt, enabled, recurring, mode, last_run)
			VALUES (?, ?, '* * * * *', '', 1, ?, 'bash', ?)`, r.id, r.id, r.recur, r.lastRun); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	deleted, err := s.DeleteFiredOneShotJobs()
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (only the fired one-shot)", deleted)
	}

	jobs, err := s.ListScheduledJobs()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	remaining := map[string]bool{}
	for _, j := range jobs {
		remaining[j.ID] = true
	}
	if remaining["fired"] {
		t.Error("the already fired one-shot survived")
	}
	if !remaining["pending"] {
		t.Error("the pending one-shot was deleted")
	}
	if !remaining["repeat"] {
		t.Error("the recurring job was deleted")
	}
}

func TestScheduledJobScriptName(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	job := ScheduledJobRow{
		ID:         "test-bundle-1",
		Name:       "telegram-poll-runner",
		Cron:       "*/13 * * * * *",
		Mode:       "bash",
		CapName:    "telegram",
		ScriptName: "telegram_poll",
		Enabled:    true,
		Recurring:  true,
	}
	if err := s.SaveScheduledJob(job); err != nil {
		t.Fatalf("save: %v", err)
	}

	jobs, err := s.ListScheduledJobs()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, j := range jobs {
		if j.ID == "test-bundle-1" {
			found = true
			if j.ScriptName != "telegram_poll" {
				t.Errorf("script_name = %q, want %q", j.ScriptName, "telegram_poll")
			}
			if j.CapName != "telegram" {
				t.Errorf("cap_name = %q, want %q", j.CapName, "telegram")
			}
		}
	}
	if !found {
		t.Error("job not found after save")
	}
}

func TestBashJobRunScriptName(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	run := BashJobRun{
		JobID:      "telegram-poll-1",
		JobName:    "telegram-poll-runner",
		CapName:    "telegram",
		ScriptName: "telegram_poll",
		Command:    "echo poll",
		Status:     "ok",
		ExitCode:   0,
	}
	if err := s.SaveBashJobRun(run); err != nil {
		t.Fatalf("save: %v", err)
	}

	runs, err := s.GetBashJobRuns("telegram-poll-1", 10)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].ScriptName != "telegram_poll" {
		t.Errorf("script_name = %q, want %q", runs[0].ScriptName, "telegram_poll")
	}
	if runs[0].CapName != "telegram" {
		t.Errorf("cap_name = %q, want %q", runs[0].CapName, "telegram")
	}
}
