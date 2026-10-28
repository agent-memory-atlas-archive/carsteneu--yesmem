package daemon

import (
	"strings"
	"testing"
)

// YM-273 (4): one project's verdicts are pruned by string prefix in proxy_state.
// A project name containing the separator therefore sits inside another project's
// prefix, and pruning "los25b" would delete the verdicts of "los25b:x".
func TestLos25b_RecurrenceVerdictPrefixIsolatesColonProjects(t *testing.T) {
	short := recurrenceVerdictPrefix("los25b")
	long := recurrenceVerdictPrefix("los25b:x")

	if short == long {
		t.Fatalf("both projects share the verdict prefix %q", short)
	}
	if strings.HasPrefix(long, short) {
		t.Fatalf("prefix of %q (%q) is contained in the prefix of %q (%q); pruning one deletes the other's verdicts",
			"los25b:x", long, "los25b", short)
	}
	// The encoding must stay injective: the escape character itself is encoded.
	if recurrenceProjectToken("a%b") == recurrenceProjectToken("a%3Ab") {
		t.Fatal("token encoding is not injective: a literal %3A collides with an escaped ':'")
	}
}

// YM-273 (4): a dismissed cluster must survive the verdict prune of a project whose
// name is a prefix of its own, otherwise that project pays for the same LLM verdict
// again on every membership change of its neighbour.
func TestLos25b_VerdictPruneKeepsTheNeighbourProjectsVerdicts(t *testing.T) {
	s := mustStore(t)
	// Paths, not bare names: a project key is a repository path (YM-424), and the
	// neighbour is still a string prefix of the longer name.
	const neighbour = "/home/testuser/projects/los25b"
	const project = "/home/testuser/projects/los25b:x"

	neighbourIDs := los25SeedRecurrenceCluster(t, s, neighbour, 3)
	los25SeedRecurrenceCluster(t, s, project, 3)

	client := &los25CountingLLM{reply: "kein_problem"}
	detectForProject(s, client, neighbour, neighbour)
	detectForProject(s, client, project, project)
	if got := client.CallCount(); got != 2 {
		t.Fatalf("setup asked the LLM %d times, want 2 (one per project)", got)
	}

	// Rotate the neighbour's membership: its stale verdict is pruned on the next run.
	for _, id := range neighbourIDs {
		if err := s.SupersedeLearning(id, 0, "los25b test: rotate cluster"); err != nil {
			t.Fatalf("supersede %d: %v", id, err)
		}
	}
	los25SeedRecurrenceCluster(t, s, neighbour, 3)
	detectForProject(s, client, neighbour, neighbour)
	if got := client.CallCount(); got != 3 {
		t.Fatalf("the neighbour's new cluster was not evaluated (calls = %d, want 3)", got)
	}

	// The other project's cluster is unchanged, so its verdict still holds and the
	// LLM is not asked again.
	detectForProject(s, client, project, project)
	if got := client.CallCount(); got != 3 {
		t.Fatalf("the neighbour's verdict prune dropped %q's verdict (calls = %d, want 3)", project, got)
	}
}
