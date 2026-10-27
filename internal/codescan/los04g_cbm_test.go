package codescan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stderrSuffix renders foreign CLI output into an error string that reaches
// daemon.log and the scan_cooldown row. Unicode line and paragraph separators
// (Zl/Zp) end a line for some readers, so they must not survive (YM-332).
func TestStderrSuffix_DropsUnicodeLineSeparators(t *testing.T) {
	got := stderrSuffix("a\u2028b\u2029c")
	if strings.ContainsAny(got, "\u2028\u2029") {
		t.Fatalf("unicode line separators survived: %q", got)
	}
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") || !strings.Contains(got, "c") {
		t.Fatalf("the reason was dropped: %q", got)
	}
}

// los04gReadCallLog reads the fake CLI's argument log; a missing file means no
// call was logged.
func los04gReadCallLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	return string(data)
}

// The repo path is handed to the CBM CLI as the value of --repo-path. A path
// starting with "-" is parsed as a flag, so it must not reach the CLI (YM-332,
// same class as the project slug guard from YM-311).
func TestCBMIndexRepository_RejectsFlagShapedRepoPath(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "fake-calls.log")
	t.Setenv("FAKE_CBM_LOG", logPath)
	bin, _ := writeFakeCBM(t)

	// A plain path first: it proves the fake CLI logs its calls, so an empty log
	// below really means the guard held instead of a silent fixture.
	if err := cbmIndexRepository(bin, "plain/path", time.Second); err != nil {
		t.Fatalf("plain path: %v", err)
	}
	if !strings.Contains(los04gReadCallLog(t, logPath), "index_repository") {
		t.Fatal("fixture broken: the fake CLI did not log its call")
	}

	if err := cbmIndexRepository(bin, "-pwned", time.Second); err == nil {
		t.Fatal("a flag-shaped repo path must fail")
	}
	if logged := los04gReadCallLog(t, logPath); strings.Contains(logged, "-pwned") {
		t.Fatalf("CBM CLI was invoked with a flag-shaped repo path:\n%s", logged)
	}
}
