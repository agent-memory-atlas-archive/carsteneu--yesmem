package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-427 [PRX-P58] (3) and (4): two cap-handler findings from the same audit.
//
//  3. acceptCapProposal picked the active cap of that name by list order. A name
//     can be live twice — bound to a project and global — so accepting a proposal
//     could rewrite the wrong body: the caller works in a project whose cap
//     shadows the global one, and the accept patched the global row.
//  4. activate_cap named the project path of the refused row in its error text,
//     which tells a caller without access where another project lives.

// los59bSeedProposalForProject stages a cap_proposed row that is bound to the
// project, the way the auto-correct loop writes it: it clones the body of the row
// the project actually runs and keeps the same binding.
func los59bSeedProposalForProject(t *testing.T, h *Handler, capName, project, proposedBash string) int64 {
	t.Helper()
	caps, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("lookup active cap %q: %v", capName, err)
	}
	var bound *models.Learning
	for i := range caps {
		meta, err := ParseCapMeta(caps[i].Context)
		if err != nil || meta.Name != capName || caps[i].Project != project {
			continue
		}
		bound = &caps[i]
		break
	}
	if bound == nil {
		t.Fatalf("no active cap %q bound to %q", capName, project)
	}
	meta, err := ParseCapMeta(bound.Context)
	if err != nil {
		t.Fatalf("parse active CapMeta: %v", err)
	}
	for i := range meta.Scripts {
		if meta.Scripts[i].Runtime == "bash" {
			meta.Scripts[i].Body = proposedBash
		}
	}
	ctxJSON, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("render proposal CapMeta: %v", err)
	}
	id, err := h.store.InsertLearning(&models.Learning{
		Content:     bound.Content + " [auto-correct proposal]",
		Category:    "cap_proposed",
		Source:      "auto_correct_proposal",
		Project:     project,
		Confidence:  1.0,
		Context:     ctxJSON,
		Domain:      bound.Domain,
		TriggerRule: "cap_proposed:" + capName,
		Keywords:    append(append([]string{}, bound.Keywords...), "pending_approval"),
		CreatedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("insert proposal: %v", err)
	}
	return id
}

// los59bCapBody returns the bash body of the active cap of that name and binding.
func los59bCapBody(t *testing.T, h *Handler, capName, project string) string {
	t.Helper()
	caps, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("lookup active cap %q: %v", capName, err)
	}
	for i := range caps {
		if caps[i].Project != project {
			continue
		}
		meta, err := ParseCapMeta(caps[i].Context)
		if err != nil || meta.Name != capName {
			continue
		}
		for _, s := range meta.Scripts {
			if s.Runtime == "bash" {
				return s.Body
			}
		}
	}
	return ""
}

// The proposal is accepted in the project whose row shadows the global cap, and
// the project row is patched. The global row of the same name must not be touched:
// rewriting it would change the cap for every other project.
func TestLos59bCapProposalAcceptTargetsTheProjectRowNotTheGlobalOne(t *testing.T) {
	h, _ := mustHandler(t)
	project := filepath.Join(t.TempDir(), "srv", "los59b-rank")
	// The bound row first, the global one second: the store answers newest-first,
	// so the global row is what a list-order pick would take.
	seedCap(t, h, "los59b_rank", project, "echo own-old\nexit 0\n", []string{"los59b_rank"})
	seedCap(t, h, "los59b_rank", "", "echo global-old\nexit 0\n", []string{"los59b_rank"})
	propID := los59bSeedProposalForProject(t, h, "los59b_rank", project, "echo proposal-new\nexit 0\n")

	resp := h.Handle(Request{Method: "cap_proposal_decide", Params: map[string]any{
		"id": float64(propID), "decision": "accept",
	}})
	if resp.Error != "" {
		t.Fatalf("accept failed: %s", resp.Error)
	}

	if body := los59bCapBody(t, h, "los59b_rank", project); !strings.Contains(body, "echo proposal-new") {
		t.Errorf("the project's own row was not patched: body=%q", body)
	}
	if body := los59bCapBody(t, h, "los59b_rank", ""); !strings.Contains(body, "echo global-old") {
		t.Errorf("the global row of the same name was rewritten: body=%q", body)
	}
}

// The refusal for a foreign cap must survive, but it may not tell a caller without
// access where that other project lives.
func TestLos59bActivateCapErrorHidesTheForeignProjectPath(t *testing.T) {
	h, _ := mustHandler(t)
	foreign := filepath.Join(t.TempDir(), "b", "los59b-foreign")
	seedCapFull(t, h, "los59b_foreign", foreign, "", "async ({ x }) => x", "", nil)
	own := filepath.Join(t.TempDir(), "a", "los59b-own")

	resp := h.handleActivateCap(map[string]any{
		"name":      "los59b_foreign",
		"thread_id": "thread-los59b",
		"project":   own,
	})

	if resp.Error == "" {
		t.Fatal("a cap of another project was activated")
	}
	if strings.Contains(resp.Error, foreign) {
		t.Errorf("the refusal names another project's path: %q", resp.Error)
	}
	if !strings.Contains(resp.Error, "another project") {
		t.Errorf("the refusal stopped saying why it refused: %q", resp.Error)
	}
}
