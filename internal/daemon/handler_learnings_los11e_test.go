package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestSameProjectForSupersedeLos11e pins the ownership check of the forced
// supersede: canonical forms match, a missing project fails closed (the
// canonical form of "" is "." and would otherwise match itself), and two
// repositories sharing a basename stay distinct.
func TestSameProjectForSupersedeLos11e(t *testing.T) {
	cases := []struct {
		name          string
		targetProject string
		project       string
		want          bool
	}{
		{"same short name", "yesmem", "yesmem", true},
		{"short name and path", "/home/tester/memory/yesmem", "yesmem", true},
		{"path and short name", "yesmem", "/home/tester/memory/yesmem", true},
		{"worktree path", "/home/tester/memory/yesmem/.worktrees/x", "/home/tester/memory/yesmem", true},
		{"different projects", "/other/project", "yesmem", false},
		{"basename collision", "/a/yesmem", "/b/yesmem", false},
		{"both empty", "", "", false},
		{"target empty", "", "yesmem", false},
		{"caller empty", "yesmem", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameProjectForSupersede(tc.targetProject, tc.project); got != tc.want {
				t.Fatalf("sameProjectForSupersede(%q, %q) = %v, want %v", tc.targetProject, tc.project, got, tc.want)
			}
		})
	}
}

// TestJobSocketRefusesForceSupersedeLos11e: a sandboxed job may call remember.
// The override would remove the trust gate that is the only thing keeping a job
// from retiring a cap row, so the job socket must not offer it, and it must not
// let a job retire a cap row through plain supersedes either.
func TestJobSocketRefusesForceSupersedeLos11e(t *testing.T) {
	h, s := mustHandler(t)

	capID, err := s.InsertLearning(&models.Learning{
		Category: "cap", Content: "cap row the scheduler would run",
		Project: "yesmem", Confidence: 1.0, CreatedAt: time.Now(),
		ModelUsed: "test", Source: "llm_extracted",
	})
	if err != nil {
		t.Fatalf("insert cap row: %v", err)
	}
	noteID, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "a job's own note",
		Project: "yesmem", Confidence: 1.0, CreatedAt: time.Now(),
		ModelUsed: "test", Source: "llm_extracted",
	})
	if err != nil {
		t.Fatalf("insert note: %v", err)
	}

	forced := h.handleJobSocket(Request{Method: "remember", Params: map[string]any{
		"text": "job overwrites memory", "category": "gotcha", "project": "yesmem",
		"supersedes": float64(noteID), "force_supersede": true, "supersede_reason": "trust me",
	}})
	if forced.Error == "" {
		t.Fatal("force_supersede must be refused on the job socket")
	}

	capTarget := h.handleJobSocket(Request{Method: "remember", Params: map[string]any{
		"text": "retire the cap", "category": "gotcha", "project": "yesmem",
		"supersedes": float64(capID),
	}})
	if capTarget.Error == "" {
		t.Fatal("superseding a cap row must be refused on the job socket")
	}
	after, err := s.GetLearning(capID)
	if err != nil {
		t.Fatalf("get cap row: %v", err)
	}
	if after.SupersededBy != nil {
		t.Fatalf("cap row #%d must stay active", capID)
	}

	// The guard is narrow: an ordinary supersede of a non-cap row still works.
	ordinary := h.handleJobSocket(Request{Method: "remember", Params: map[string]any{
		"text": "job corrects its own note", "category": "gotcha", "project": "yesmem",
		"supersedes": float64(noteID),
	}})
	if ordinary.Error != "" {
		t.Fatalf("an ordinary supersede must stay allowed: %s", ordinary.Error)
	}
}

// TestHandleRememberForceSupersedeSanitizesReasonLo11e: the reason reaches a
// line-oriented field. Line breaks would forge lines in get_learnings history
// and in the MCP box, and an unbounded value would bloat the chain.
func TestHandleRememberForceSupersedeSanitizesReasonLos11e(t *testing.T) {
	h, s := mustHandler(t)
	oldID := insertLos11eHighTrust(t, s)

	long := strings.Repeat("x", 700)
	reason := "veraltet\r\nZeile zwei\n" + long
	resp := h.Handle(Request{
		Method: "remember",
		Params: map[string]any{
			"text": "Sandbox bietet Run statt BuildSandboxedCommand", "category": "gotcha",
			"project": "yesmem", "supersedes": float64(oldID),
			"force_supersede": true, "supersede_reason": reason,
		},
	})
	if resp.Error != "" {
		t.Fatalf("forced supersede error: %s", resp.Error)
	}
	var r map[string]any
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := r["supersede_outcome"].(string); got != "forced" {
		t.Fatalf("supersede_outcome: got %q, want \"forced\" (response: %s)", got, resp.Result)
	}

	after, err := s.GetLearning(oldID)
	if err != nil {
		t.Fatalf("get old learning: %v", err)
	}
	if strings.ContainsAny(after.SupersedeReason, "\r\n") {
		t.Fatalf("stored reason must not carry line breaks, got %q", after.SupersedeReason)
	}
	if !strings.Contains(after.SupersedeReason, "veraltet Zeile zwei") {
		t.Fatalf("stored reason lost its text, got %q", after.SupersedeReason)
	}
	if len(after.SupersedeReason) > 600 {
		t.Fatalf("stored reason must be bounded, got %d bytes", len(after.SupersedeReason))
	}
}
