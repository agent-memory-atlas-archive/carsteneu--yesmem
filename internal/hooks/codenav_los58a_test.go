package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-425: dismiss_code_nav answers {"status":"ok"}, so the next navigation must
// not be blocked again. The block message advertises that escape, therefore one
// call has to lift it for the session. The store keeps counting; the threshold is
// this call site's decision.
func TestLos58aOneDismissLiftsTheCodeNavBlock(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	const session = "opencode:ses_los58a"
	const cmd = `grep -rn "CanonicalProject(" internal/`
	indexed := func(project, path string) bool { return true }

	if _, block := CheckCodeNav(cmd, "/proj", "yesmem", session, indexed, store.CodeNavDismissed(session)); !block {
		t.Fatal("the guard must block before any dismissal")
	}
	if err := store.DismissCodeNav(session); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if _, block := CheckCodeNav(cmd, "/proj", "yesmem", session, indexed, store.CodeNavDismissed(session)); block {
		t.Fatal("dismiss_code_nav answered ok, so one call must lift the block")
	}
}

// The block message has to name an escape that works. The retry is the escape
// the opencode plugin implements; the Claude Code hook lifts the block through
// dismiss_code_nav, so its message must name that call (YM-425).
func TestLos58aBlockMessageNamesTheWorkingEscape(t *testing.T) {
	reason, block := CheckCodeNav(
		`grep -n "func Run" internal/proxy/proxy.go`,
		"/proj",
		"yesmem",
		"session-123",
		func(project, path string) bool { return true },
		false,
	)
	if !block {
		t.Fatal("should block an indexed file")
	}
	if !strings.Contains(reason, "dismiss_code_nav") {
		t.Errorf("the block message must name dismiss_code_nav as the escape, got: %s", reason)
	}
	// The call-site question is what a grep usually asks, and search_code answers
	// it (measured: 60 matches in 20 locations with line numbers). The advice must
	// name that tool for call sites, not only the per-file symbol tools.
	if !strings.Contains(reason, "Call sites") || !strings.Contains(reason, `search_code("<symbol>("`) {
		t.Errorf("the block message must name search_code for call sites, got: %s", reason)
	}
}

// YM-409: the shell line the opencode plugin splits at its operators was a single
// command to the Go hook, so `cd x && grep …` was read as "cd" and never
// classified. The hook must follow the directory change and judge the file the
// command really names.
func TestLos58aCheckCodeNavCDChainBlocks(t *testing.T) {
	var probed []string
	reason, block := CheckCodeNav(
		`cd internal && grep -n Run hooks/check.go`,
		"/proj",
		"yesmem",
		"session-123",
		func(project, path string) bool {
			probed = append(probed, path)
			return path == "internal/hooks/check.go"
		},
		false,
	)
	if !block {
		t.Fatalf("a navigation command behind cd must block; probed %v", probed)
	}
	if !strings.Contains(reason, "hooks/check.go") {
		t.Errorf("reason should name the file, got: %s", reason)
	}
}

// pushd/popd move the working directory for the following segments only; the file
// after popd belongs to the original directory (the plugin's navTargets does the
// same, YM-84).
func TestLos58aNavTargetsFollowPushdPopd(t *testing.T) {
	var probed []string
	CheckCodeNav(
		`pushd sub && cat a.go && popd && cat b.go`,
		"/proj",
		"yesmem",
		"session-123",
		func(project, path string) bool {
			probed = append(probed, path)
			return false
		},
		false,
	)
	want := []string{"sub/a.go", "b.go"}
	if len(probed) != len(want) {
		t.Fatalf("probed %v, want %v", probed, want)
	}
	for i := range want {
		if probed[i] != want[i] {
			t.Fatalf("probed %v, want %v", probed, want)
		}
	}
}

// The recursive search over a directory is the normal call-site command. This
// pins the operand shape: "internal/" must reach the index predicate, and a path
// with a trailing separator must not turn into a different question. That the
// store then answers for a directory is proven by
// TestLos58aIsTreeInCodeIndexDirectory (internal/storage), which is what the hook
// now passes as isIndexed (YM-409).
func TestLos58aCheckCodeNavDirectoryOperandBlocks(t *testing.T) {
	reason, block := CheckCodeNav(
		`grep -rn "CanonicalProject(" internal/`,
		"/proj",
		"yesmem",
		"session-123",
		func(project, path string) bool { return path == "internal" || path == "internal/" },
		false,
	)
	if !block {
		t.Fatal("a recursive grep on an indexed directory must block like it does in the plugin")
	}
	if reason == "" {
		t.Error("should return a reason")
	}
}

// find names a directory, and the plugin classifies it (blockedCommands includes
// find). Without the tool in the Go list the command passed untouched (YM-409).
func TestLos58aCheckCodeNavFindBlocks(t *testing.T) {
	_, block := CheckCodeNav(
		`find internal/ -name "*.go"`,
		"/proj",
		"yesmem",
		"session-123",
		func(project, path string) bool { return path == "internal/" || path == "internal" },
		false,
	)
	if !block {
		t.Fatal("find on an indexed directory must block like it does in the plugin")
	}
}

// A source extension outside a short allowlist must not exempt a file: the
// codescan index knows 33 suffixes, and before the review an argument filter with
// 15 of them silently stopped blocking `.tsx`, `.kt` and the rest (YM-409).
func TestLos58aCheckCodeNavBlocksEverySourceSuffix(t *testing.T) {
	for _, file := range []string{"tui.tsx", "main.kt", "app.rb", "run.sh", "x.go"} {
		seen := ""
		_, block := CheckCodeNav(
			"cat "+file,
			"/proj",
			"yesmem",
			"session-123",
			func(project, path string) bool {
				seen = path
				return true
			},
			false,
		)
		if !block {
			t.Errorf("cat %s must reach the index lookup, probed %q", file, seen)
		}
	}
}

// The most common call-site form names a directory without a separator, and the
// root operand is a directory as well. Both must reach the index (YM-409).
func TestLos58aCheckCodeNavDirectoryFormsReachTheIndex(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want string
	}{
		{`grep -rn needle internal`, "internal"},
		{`grep -rn needle .`, "."},
		{`grep -rn needle .github/workflows`, ".github/workflows"},
	} {
		probed := []string{}
		CheckCodeNav(tc.cmd, "/proj", "yesmem", "session-123", func(project, path string) bool {
			probed = append(probed, path)
			return true
		}, false)
		if len(probed) != 1 || probed[0] != tc.want {
			t.Errorf("%q: probed %v, want [%s]", tc.cmd, probed, tc.want)
		}
	}
}

// A navigation target outside the working directory is not a project file. The
// earlier hook cut the line at "&&", so it blocked /elsewhere files as if they
// belonged to the project; the plugin always skipped them (YM-409).
func TestLos58aOutsideTargetDoesNotBlock(t *testing.T) {
	probed := []string{}
	_, block := CheckCodeNav(
		`cd /elsewhere && cat x.go`,
		"/proj",
		"yesmem",
		"session-123",
		func(project, path string) bool {
			probed = append(probed, path)
			return true
		},
		false,
	)
	if block {
		t.Fatalf("a file outside the project must not block; probed %v", probed)
	}
	if len(probed) != 0 {
		t.Fatalf("an outside target must not be looked up at all, probed %v", probed)
	}
}

// The wiring, not a stub: the predicate the hook passes must answer for a
// directory. Without this binding a switch back to the file-only lookup would
// leave every test green (YM-409).
func TestLos58aCodeNavIndexerAnswersForDirectories(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.PersistScan("yesmem", `{"Packages":[{"Files":[{"Path":"internal/hooks/check.go"}]}]}`, "head", 0); err != nil {
		t.Fatalf("persist scan: %v", err)
	}
	indexed := codeNavIndexer(store)

	if !indexed("yesmem", "internal") {
		t.Error("the hook's predicate must answer for a directory operand")
	}
	if !indexed("yesmem", "internal/hooks/check.go") {
		t.Error("the hook's predicate must keep answering for files")
	}
	if indexed("yesmem", "docs") {
		t.Error("a directory without indexed files must not answer true")
	}
}

// The block message embeds the file and the session id in a quoted pseudo-call.
// A repository can hold a file whose name closes that quote; the plugin strips
// such characters, the Go hook must do the same (YM-425 review).
func TestLos58aBlockMessageStripsControlCharacters(t *testing.T) {
	reason, block := CheckCodeNav(
		"cat 'weird\"name\u2028.go'",
		"/proj",
		"yesmem",
		"ses_1",
		func(project, path string) bool { return true },
		false,
	)
	if !block {
		t.Fatal("should block")
	}
	// The pseudo-call itself carries quotes; what must not survive is a quote,
	// backtick or line separator out of the VALUE.
	if strings.Contains(reason, "\u2028") {
		t.Errorf("line separators must not reach the message, got: %q", reason)
	}
	if strings.Contains(reason, `weird"name`) {
		t.Errorf("a quote out of the file name must not reach the message, got: %q", reason)
	}
	if !strings.Contains(reason, "weirdname.go") {
		t.Errorf("the sanitized file name should still be named, got: %q", reason)
	}
}

// The fixtures are shared with the opencode plugin's test suite: the same shell
// line must resolve to the same targets on both sides, otherwise a command
// blocks on one side and slips through on the other (YM-409).
func TestLos58aNavTargetsMatchSharedFixtures(t *testing.T) {
	type expectedTarget struct {
		Tool string `json:"tool"`
		File string `json:"file"`
	}
	var fixture struct {
		Cwd   string `json:"cwd"`
		Cases []struct {
			Command  string           `json:"command"`
			Expected []expectedTarget `json:"expected"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "codenav_parity.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("fixture file carries no cases")
	}

	for _, tc := range fixture.Cases {
		targets := ParseNavTargets(tc.Command, fixture.Cwd)
		if len(targets) != len(tc.Expected) {
			t.Fatalf("%q: got %v, want %v", tc.Command, targets, tc.Expected)
		}
		for i, want := range tc.Expected {
			if targets[i].Tool != want.Tool || targets[i].File != want.File {
				t.Fatalf("%q: target %d = %v, want %v", tc.Command, i, targets[i], want)
			}
		}
	}
}
