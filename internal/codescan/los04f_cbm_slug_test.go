package codescan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The project slug handed to the CBM CLI in cbm-gc comes from a cache file name,
// which anyone with write access to the cache directory chooses. A leading "-"
// would be parsed by the CLI as a flag instead of a project name (YM-311,
// escaped from YM-277).
func TestCBMCallsRejectFlagShapedProjectSlug(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "fake-calls.log")
	t.Setenv("FAKE_CBM_LOG", logPath)
	bin, _ := writeFakeCBM(t)

	// A usable slug first: it proves the fake CLI logs its calls, so an empty
	// log below really means the guard held instead of a silent fixture.
	cbmIndexRootPath(bin, "plain-slug")
	logged := los04fReadCallLog(t, logPath)
	if !strings.Contains(logged, "index_status") {
		t.Fatalf("fixture broken: the fake CLI did not log its call:\n%s", logged)
	}

	if got := cbmIndexRootPath(bin, "-pwned"); got != "" {
		t.Fatalf("index_status of a flag-shaped slug returned %q, want no result", got)
	}
	if _, err := cbmQuery(bin, "-pwned", "MATCH (n) RETURN n"); err == nil {
		t.Fatal("query with a flag-shaped project slug must fail")
	}

	logged = los04fReadCallLog(t, logPath)
	if strings.Contains(logged, "-pwned") {
		t.Fatalf("CBM CLI was invoked with a flag-shaped project slug:\n%s", logged)
	}
	if got := strings.Count(logged, "index_status"); got != 1 {
		t.Fatalf("index_status was called %d times, want only the plain-slug call:\n%s", got, logged)
	}
}

func los04fReadCallLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	return string(data)
}
