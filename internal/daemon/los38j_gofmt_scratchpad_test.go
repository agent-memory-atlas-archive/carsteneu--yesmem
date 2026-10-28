package daemon

import (
	"bytes"
	"go/format"
	"os"
	"testing"
)

// gofmt drift is invisible to the compiler: handler_scratchpad.go shipped with an
// over-aligned key block that only a formatter run reports, so the next reader
// sees a file that differs from every other file in the package for no reason
// (YM-299). The check is pinned on the file this ticket touched; a repository-wide
// formatter gate is a separate decision.
func TestLos38jScratchpadHandlerIsGofmtClean(t *testing.T) {
	const file = "handler_scratchpad.go"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	want, err := format.Source(src)
	if err != nil {
		t.Fatalf("format %s: %v", file, err)
	}
	if !bytes.Equal(src, want) {
		t.Fatalf("%s is not gofmt-clean; run gofmt -w %s", file, file)
	}
}
