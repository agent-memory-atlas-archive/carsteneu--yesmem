package daemon

import (
	"bytes"
	"go/format"
	"os"
	"testing"
)

// gofmt drift is invisible to the compiler. handler_docs.go shipped on main with
// over-aligned map keys and struct literals, so it reads as a file that differs
// from the rest of the package for no reason (YM-329 [DEV-DT51]). Pinned on the
// file this ticket touched; a repository-wide formatter gate is a separate
// decision (see TestLos38jScratchpadHandlerIsGofmtClean).
func TestLos07eDocsHandlerIsGofmtClean(t *testing.T) {
	const file = "handler_docs.go"
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
