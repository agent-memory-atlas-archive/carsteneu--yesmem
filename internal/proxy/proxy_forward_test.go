package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteDebugDump_DisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YESMEM_PROXY_DEBUG", "")

	if got := writeDebugDump(dir, "sess-1", dumpKindBody, 42, []byte(`{"hi":1}`)); got != "" {
		t.Errorf("expected no dump path, got %q", got)
	}

	if _, err := os.Stat(filepath.Join(dir, "debug")); !os.IsNotExist(err) {
		t.Fatalf("expected no debug dir when YESMEM_PROXY_DEBUG unset, got err=%v", err)
	}
}

func TestWriteDebugDump_Enabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YESMEM_PROXY_DEBUG", "1")

	path := writeDebugDump(dir, "sess-1", dumpKindBody, 7, []byte(`{"hi":1}`))
	if path == "" {
		t.Fatal("expected a dump path")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dump %s: %v", path, err)
	}
	if string(got) != `{"hi":1}` {
		t.Errorf("dump body mismatch: got %q want %q", got, `{"hi":1}`)
	}
	if filepath.Dir(path) != filepath.Join(dir, dumpDirName) {
		t.Errorf("dump written to %s, want it inside %s", path, filepath.Join(dir, dumpDirName))
	}
}

func TestWriteDebugDump_OtherValueIsOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YESMEM_PROXY_DEBUG", "true")

	writeDebugDump(dir, "sess-1", dumpKindBody, 1, []byte("x"))

	if _, err := os.Stat(filepath.Join(dir, "debug")); !os.IsNotExist(err) {
		t.Fatalf("expected no dump for YESMEM_PROXY_DEBUG=true (only \"1\" enables), got err=%v", err)
	}
}

func TestWriteDebugDump_EmptyDataDir(t *testing.T) {
	t.Setenv("YESMEM_PROXY_DEBUG", "1")

	if got := writeDebugDump("", "sess-1", dumpKindBody, 99, []byte("x")); got != "" {
		t.Errorf("expected no dump path for an empty data dir, got %q", got)
	}
}
