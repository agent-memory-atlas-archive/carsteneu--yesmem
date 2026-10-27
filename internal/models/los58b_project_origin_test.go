package models

import "testing"

// YM-424: a bare name from the old key space names a project it cannot be resolved
// to, and an empty value names none at all. Both used to collapse into the same
// basename-shaped string, so no consumer could tell an unknown project from a
// global row.
func TestLos58bProjectOriginSeparatesTheThreeStates(t *testing.T) {
	tests := []struct {
		project string
		want    ProjectOrigin
	}{
		{"", ProjectOriginUnassigned},
		{"/home/tester/memory/yesmem", ProjectOriginPath},
		{"/home/tester/memory/yesmem/.worktrees/yesloop-los-58b", ProjectOriginPath},
		{"yesmem", ProjectOriginLegacy},
		{"yesloop-sealdb-export-migration", ProjectOriginLegacy},
		{".worktrees/yesloop-los-58b", ProjectOriginLegacy},
	}
	for _, tt := range tests {
		if got := ClassifyProject(tt.project); got != tt.want {
			t.Errorf("ClassifyProject(%q) = %v, want %v", tt.project, got, tt.want)
		}
	}
}
