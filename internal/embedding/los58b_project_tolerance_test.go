package embedding

import "testing"

// YM-424: the vector store keeps its own copy of the tolerance rule (it runs without a
// storage handle). A row without a path stays discoverable under a request of the same
// basename; a row that names its repository by path is compared through its cluster key,
// so a worktree belongs to its repository while a different repository with the same
// basename does not.
func TestLos58bProjectFilterSeparatesTheThreeStates(t *testing.T) {
	const repo = "/home/tester/memory/yesmem"
	const worktree = repo + "/.worktrees/yesloop-los-58b"
	tests := []struct {
		name      string
		stored    string
		requested string
		want      bool
	}{
		{"unassigned row stays discoverable everywhere", "", repo, true},
		{"legacy row resolves by its basename", "yesmem", "/home/tester/projects/yesmem", true},
		{"legacy row does not resolve to another name", "yesmem", "/home/tester/projects/other", false},
		{"path row matches its own project", repo, repo, true},
		{"path row of another repo with the same basename", repo, "/home/tester/projects/yesmem", false},
		{"path row vs a bare-name request", repo, "yesmem", true},
		{"repository row is visible from its worktree", repo, worktree, true},
		{"worktree row is visible from the repository", worktree, repo, true},
	}
	for _, tt := range tests {
		if got := projectMatchesTolerant(tt.stored, tt.stored, tt.requested); got != tt.want {
			t.Errorf("%s: projectMatchesTolerant(%q, %q, %q) = %v, want %v",
				tt.name, tt.stored, tt.stored, tt.requested, got, tt.want)
		}
	}
}
