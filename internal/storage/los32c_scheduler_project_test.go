package storage

import "testing"

// A scheduled job belongs to the project it was created from. The cap that runs
// in it has to name a project to the daemon, and a bare basename is refused once
// two projects share it — so the job row has to keep the caller's project.
func TestLos32cScheduledJobKeepsItsProject(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	const project = "/home/tester/memory/yesmem"
	if err := s.SaveScheduledJob(ScheduledJobRow{
		ID: "sched-los32c-storage", Name: "los32c-storage", Cron: "*/5 * * * *",
		Mode: "bash", CapName: "proxy_health", Project: project,
		Enabled: true, Recurring: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	jobs, err := s.ListScheduledJobs()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].Project != project {
		t.Fatalf("project = %q, want %q", jobs[0].Project, project)
	}
}
