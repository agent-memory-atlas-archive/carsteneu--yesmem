package daemon

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

func TestEmbeddingCacheModelKey_Nil(t *testing.T) {
	if got := embeddingCacheModelKey(nil); got != "unknown" {
		t.Errorf("expected 'unknown', got %q", got)
	}
}

func TestEmbeddingCacheModelKey_WithProvider(t *testing.T) {
	cfg := &config.Config{}
	cfg.Embedding.Provider = "openai"
	if got := embeddingCacheModelKey(cfg); got != "openai" {
		t.Errorf("expected 'openai', got %q", got)
	}
}

func TestEmbeddingCacheModelKey_EmptyProvider(t *testing.T) {
	cfg := &config.Config{}
	if got := embeddingCacheModelKey(cfg); got != "unknown" {
		t.Errorf("expected 'unknown', got %q", got)
	}
}

// The former findGitRoot tests covered the walk-up-to-enclosing-repo behaviour
// that let a doc-sync pull the user's working repository (YM-27). Repo detection
// for doc sources is now repo.CloneRoot (internal/repo) — a repo root or nothing
// — covered by TestCloneRoot there and end-to-end by
// TestTryGitPull_DoesNotPullSourceInsideWorkingRepo.

func TestSocketPath(t *testing.T) {
	got := SocketPath("/tmp/yesmem-test")
	expected := "/tmp/yesmem-test/daemon.sock"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}
