package opencode

import (
	"context"
	"os"
	"testing"
	"time"
)

// liveE2EEnv opts into the read-only acceptance run against the developer's
// RUNNING opencode services. Off by default: unit tests must never depend on
// (or disturb) a live installation.
const liveE2EEnv = "YESMEM_LIVE_OPENCODE_E2E"

// liveCases are the acceptance fixtures of 2026-10-15: the workdir and section
// of yesloop agent-20261015-12 (whose session id capture was failing, the
// defect this client exists for) and of the agent that wrote this test.
var liveCases = []struct {
	name    string
	workDir string
	section string
	wantID  string
}{
	{
		name:    "spawned yesloop agent-20261015-12",
		workDir: "/home/tester/projects/opencode/.worktrees/v2-code-fence-default",
		section: "yesloop-v2-code-fence-default",
		wantID:  "ses_ec0302557ffe0NPvG6GRwmq7Qd",
	},
	{
		name:    "this loop's own worktree",
		workDir: "/home/tester/memory/yesmem/.worktrees/yesloop-v2-session-capture",
		section: "yesloop-v2-session-capture",
		wantID:  "ses_ebfd8cc9affe1NblyqKv6uBBU0",
	},
}

// TestLiveOpencodeV2_ResolvesSpawnedAgentSessions is the end-to-end acceptance
// proof: against the LIVE server API (read-only GETs only), the client resolves
// the session id of both a daemon-spawned agent and this session from nothing
// but the workdir, the section and a lower time bound — and the result is the
// id the API reports for that session, not merely some session.
func TestLiveOpencodeV2_ResolvesSpawnedAgentSessions(t *testing.T) {
	if os.Getenv(liveE2EEnv) == "" {
		t.Skipf("set %s=1 to run against the live opencode services", liveE2EEnv)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := FirstLiveService(home)
	if err != nil {
		t.Fatalf("no live opencode v2 service: %v", err)
	}
	t.Logf("live service %s (%s)", svc.URL, svc.Version)
	c := NewClient(svc)
	ctx := context.Background()

	for _, tc := range liveCases {
		t.Run(tc.name, func(t *testing.T) {
			// Ground truth straight from the API: this session must be a root
			// session in exactly that directory.
			truth, err := c.GetSession(ctx, tc.wantID)
			if err != nil {
				t.Fatalf("GetSession(%s): %v", tc.wantID, err)
			}
			if truth.ParentID != "" {
				t.Fatalf("session %s carries parentID %q — a subagent must never be bound", tc.wantID, truth.ParentID)
			}
			if truth.Directory != tc.workDir {
				t.Fatalf("session %s is in %q, not %q", tc.wantID, truth.Directory, tc.workDir)
			}
			if truth.Created == 0 {
				t.Fatal("the live API must report time.created — the capture time bound depends on it")
			}

			// Now reproduce it the way the daemon does, bounded by the spawn
			// time (here: the session's own creation time).
			got, err := c.FindSession(ctx, MarkerQuery{
				Directory: tc.workDir, Section: tc.section,
				Since: time.UnixMilli(truth.Created),
			})
			if err != nil {
				t.Fatalf("FindSession: %v", err)
			}
			if got != tc.wantID {
				t.Fatalf("resolved %q, want %q", got, tc.wantID)
			}

			// And the same query one millisecond later must NOT match: the
			// session predates that spawn.
			got, err = c.FindSession(ctx, MarkerQuery{
				Directory: tc.workDir, Section: tc.section,
				Since: time.UnixMilli(truth.Created + 1),
			})
			if err != nil {
				t.Fatalf("FindSession (strict bound): %v", err)
			}
			if got != "" {
				t.Fatalf("a session created before the bound must not be bound, got %q", got)
			}
		})
	}
}

// TestLiveOpencodeV2_ServicesShareOneStore pins the multi-server finding: every
// registered service answers with the same session for the same directory,
// because they share one session store. Discovery therefore only has to find
// ONE reachable service — there is no per-server session list to merge.
func TestLiveOpencodeV2_ServicesShareOneStore(t *testing.T) {
	if os.Getenv(liveE2EEnv) == "" {
		t.Skipf("set %s=1 to run against the live opencode services", liveE2EEnv)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	svcs := DiscoverServices(home)
	if len(svcs) == 0 {
		t.Skip("no opencode service registrations")
	}
	tc := liveCases[0]
	ctx := context.Background()
	live := 0
	for _, svc := range svcs {
		c := NewClient(svc)
		if _, err := c.Info(ctx); err != nil {
			t.Logf("service %s (%s) unreachable: %v", svc.URL, svc.Version, err)
			continue
		}
		live++
		got, err := c.GetSession(ctx, tc.wantID)
		if err != nil {
			t.Errorf("service %s: %v", svc.URL, err)
			continue
		}
		if got.ID != tc.wantID || got.Directory != tc.workDir {
			t.Errorf("service %s reports %+v, want %s in %s — the services do not share one store",
				svc.URL, got, tc.wantID, tc.workDir)
		}
	}
	if live == 0 {
		t.Fatal("no reachable service among the registrations")
	}
	t.Logf("%d live service(s) agree on the same session", live)
}

// TestLiveOpencodeV2_FindsForeignSession covers the second half of the goal:
// sessions the daemon did not start are discoverable too. The orchestrator
// session is addressed by id only.
func TestLiveOpencodeV2_FindsForeignSession(t *testing.T) {
	if os.Getenv(liveE2EEnv) == "" {
		t.Skipf("set %s=1 to run against the live opencode services", liveE2EEnv)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := FirstLiveService(home)
	if err != nil {
		t.Fatalf("no live opencode v2 service: %v", err)
	}

	const foreign = "ses_ec0a7fe35ffeOEy3qIGy8ViecG" // orchestrator session
	s, err := NewClient(svc).GetSession(context.Background(), foreign)
	if err != nil {
		t.Fatalf("GetSession(%s): %v", foreign, err)
	}
	if s.ID != foreign || s.Directory == "" {
		t.Fatalf("unexpected foreign session row: %+v", s)
	}
	t.Logf("foreign session %s in %s (title %q)", s.ID, s.Directory, s.Title)
}
