package daemon

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// The daemon start read the exclusion list at two places and guarded the config
// only at one of them. config.Load cannot return nil today — every error path
// falls back to Default — so the asymmetry was latent, not live. The helper keeps
// the read in one guarded place, and this test pins that the nil case stays
// handled there instead of at the call sites.
func TestExcludeProjectsFor_NilConfig(t *testing.T) {
	if got := excludeProjectsFor(nil); got != nil {
		t.Errorf("exclusions of a nil config: got %+v, want none", got)
	}
}

func TestExcludeProjectsFor_ReturnsConfiguredList(t *testing.T) {
	cfg := config.Default()
	cfg.ExcludeProjects = []string{"/home/test/excluded", "/home/test/other"}

	got := excludeProjectsFor(cfg)
	if len(got) != 2 || got[0] != "/home/test/excluded" || got[1] != "/home/test/other" {
		t.Errorf("exclusions: got %+v, want the configured list", got)
	}
}
