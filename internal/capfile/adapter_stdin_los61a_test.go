package capfile

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// los61aWorkerStub answers one json_cli request without starting a real
// worker: the wrapper writes its request into a file and reads the reply line
// from a prefilled one.
const los61aWorkerReply = `{"ok":true,"exit":0,"output_len":0}`

// los61aBash returns a bash that can run the generated adapter at all: its
// preamble opens a named coproc and allocates the two fds through variable
// redirections, both bash 4 features. Under the bash 3.2 macOS ships as
// /bin/bash the script does not even parse, so every call ends with exit 2
// before the wrapper is reached. A host with a newer bash (brew) runs them.
func los61aBash(t *testing.T) string {
	t.Helper()
	const probe = "coproc YM { :; }; exec {YM_IN}>&1; exec {YM_IN}>&-"
	for _, name := range []string{"bash", "/opt/homebrew/bin/bash", "/usr/local/bin/bash"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if exec.Command(path, "-c", probe).Run() == nil {
			return path
		}
	}
	t.Skip("no bash 4+ here: the generated adapter opens a named coproc, which the bash 3.2 on macOS cannot parse")
	return ""
}

// los61aRunAdapter runs call with the generated bash adapter prepended. stdin
// may be nil, a pipe or a socket; the run is capped so a blocking read shows up
// as a deadline error instead of a hanging test. prelude runs after the stub
// wiring and before the adapter, e.g. to close fd 0.
func los61aRunAdapter(t *testing.T, stdin *os.File, prelude, call string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	reqFile := filepath.Join(dir, "worker.in")
	if err := os.WriteFile(reqFile, nil, 0o600); err != nil {
		t.Fatalf("write worker.in: %v", err)
	}
	replyFile := filepath.Join(dir, "worker.out")
	if err := os.WriteFile(replyFile, []byte(los61aWorkerReply+"\n"), 0o600); err != nil {
		t.Fatalf("write worker.out: %v", err)
	}

	script := "set -u\n" +
		"exec 3>>\"$LOS61A_REQ\"\n" +
		"exec 4<\"$LOS61A_REPLY\"\n" +
		"YESMEM_WORKER_IN=3 YESMEM_WORKER_OUT=4\n" +
		GenerateAdapterBash() +
		prelude + call + "\n"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, los61aBash(t), "-c", script)
	cmd.Env = append(os.Environ(), "LOS61A_REQ="+reqFile, "LOS61A_REPLY="+replyFile)
	cmd.Stdin = stdin
	runErr := cmd.Run()
	req, readErr := os.ReadFile(reqFile)
	if readErr != nil {
		t.Fatalf("read worker request: %v", readErr)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return string(req), fmt.Errorf("blocked on stdin: %s did not return within 5s", call)
	}
	return string(req), runErr
}

// los61aSocketStdin returns fd 0 backed by a connected unix socket: it is
// neither a pipe nor a regular file and never reaches EOF, so any read on it
// blocks until the peer closes. The short temp dir keeps the socket path under
// the sun_path limit.
func los61aSocketStdin(t *testing.T) *os.File {
	t.Helper()
	dir, err := os.MkdirTemp("", "los61a")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		ln.Close()
		t.Fatalf("dial unix: %v", err)
	}
	f, err := conn.(*net.UnixConn).File()
	if err != nil {
		conn.Close()
		ln.Close()
		t.Fatalf("unwrap conn fd: %v", err)
	}
	t.Cleanup(func() {
		f.Close()
		conn.Close()
		ln.Close()
	})
	return f
}

func TestLos61aJSONWrapperTreatsNonPipeStdinAsEmpty(t *testing.T) {
	req, err := los61aRunAdapter(t, los61aSocketStdin(t), "", `yesmem json -r '.a'`)
	if err != nil {
		t.Fatalf("yesmem json with a non-pipe stdin: %v", err)
	}
	if !strings.Contains(req, `"stdin_len":0`) {
		t.Errorf("non-pipe stdin must be treated as empty, request was %q", req)
	}
}

// A closed fd 0 must not fall into the read: bash hands the free fd to the
// command-substitution pipe, so cat would read its own output pipe forever.
func TestLos61aJSONWrapperHandlesClosedStdin(t *testing.T) {
	req, err := los61aRunAdapter(t, nil, "exec 0<&-\n", `yesmem json -r '.a'`)
	if err != nil {
		t.Fatalf("yesmem json with a closed stdin: %v", err)
	}
	if !strings.Contains(req, `"stdin_len":0`) {
		t.Errorf("a closed fd 0 must count as an empty stdin, request was %q", req)
	}
}

func TestLos61aJSONWrapperHandlesDevNullStdin(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { devNull.Close() })

	req, err := los61aRunAdapter(t, devNull, "", `yesmem json -r '.a'`)
	if err != nil {
		t.Fatalf("yesmem json with %s as stdin: %v", os.DevNull, err)
	}
	if !strings.Contains(req, `"stdin_len":0`) {
		t.Errorf("%s must count as an empty stdin, request was %q", os.DevNull, req)
	}
}

func TestLos61aJSONWrapperStillReadsPipedStdin(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := pw.WriteString(`{"a":1}`); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	pw.Close()
	t.Cleanup(func() { pr.Close() })

	req, err := los61aRunAdapter(t, pr, "", `yesmem json -r '.a'`)
	if err != nil {
		t.Fatalf("yesmem json with a piped stdin: %v", err)
	}
	if !strings.Contains(req, `"stdin_len":7`) {
		t.Errorf("piped stdin must still be forwarded, request was %q", req)
	}
}

func TestLos61aRocketchatPollSetsStdinToDevNull(t *testing.T) {
	md, err := os.ReadFile("../../caps/bundled-caps/rocketchat/CAP.md")
	if err != nil {
		t.Fatalf("read cap: %v", err)
	}
	body := los61aScriptBlock(t, string(md), "rocketchat_poll")
	// Comment lines mention yesmem json too; only executable lines count.
	var code []string
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "#") {
			continue
		}
		code = append(code, ln)
	}
	exec := strings.Join(code, "\n")
	set := strings.Index(exec, "exec 0</dev/null")
	if set < 0 {
		t.Fatal("rocketchat_poll must set stdin to /dev/null; the scheduled job inherits an fd that never reaches EOF (YM-439)")
	}
	// The line is pointless once a yesmem json call has already run.
	if first := strings.Index(exec, "yesmem json"); first >= 0 && set > first {
		t.Error("exec 0</dev/null must precede the first yesmem json call")
	}
}

// los61aScriptBlock returns the bash body of the named cap script.
func los61aScriptBlock(t *testing.T, md, name string) string {
	t.Helper()
	head := "### " + name + "\n"
	start := strings.Index(md, head)
	if start < 0 {
		t.Fatalf("script %s not found in cap", name)
	}
	rest := md[start+len(head):]
	if next := strings.Index(rest, "\n### "); next >= 0 {
		rest = rest[:next]
	}
	fence := strings.Index(rest, "```bash\n")
	if fence < 0 {
		t.Fatalf("script %s has no bash block", name)
	}
	rest = rest[fence+len("```bash\n"):]
	if end := strings.Index(rest, "```"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}
