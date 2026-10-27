package capfile

import (
	"os"
	"testing"
)

// YM-446 (OPS-112, key 2): the bundled rocketchat cap and the installed copy had
// drifted into two lines, v14 in the repository against v15 on disk. A bundled
// revision only reaches the install when it outruns the deployed one
// (daemon/cap_sync.go skips disk >= db, the import gate rejects an older source
// revision), so the reconciled body has to carry a revision above the live v15.
// Without it the merged body is inert after every rollout.
func TestLos62bRocketchatCapOutrunsTheInstalledRevision(t *testing.T) {
	md, err := os.ReadFile("../../caps/bundled-caps/rocketchat/CAP.md")
	if err != nil {
		t.Fatalf("read cap: %v", err)
	}
	cf, err := Parse(md)
	if err != nil {
		t.Fatalf("parse cap: %v", err)
	}
	if cf.Version <= 15 {
		t.Fatalf("bundled rocketchat cap is v%d; the install carries v15 and would skip it", cf.Version)
	}
}
