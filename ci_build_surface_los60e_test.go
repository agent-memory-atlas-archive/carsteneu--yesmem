package main

// Guards for the build surface (YM-434, YM-438): `make test` must drive the
// plugin suite, and a race gate with an explicit timeout must exist locally and
// in CI, with a restrictive permissions block.
//
// These are structure scans: they parse Makefile and ci.yml and run neither bun
// nor go test. They are therefore not forgery-proof, and the review found the
// holes worth closing:
//   - a recipe line that swallows its exit status ("|| true") counts as a
//     swallowed failure, not as coverage (ciSwallowsFailure from the plugin
//     guard is reused for exactly that);
//   - every race command is checked individually, not "some line contains
//     -race", so dropping -race from one of two commands turns the guard red;
//   - comment lines are stripped before matching, so commenting a command out
//     does not pass as running it;
//   - the -run selection has to match in BOTH directions between Makefile and
//     ci.yml, because a one-sided check lets the local gate shrink silently;
//   - every name in the selection must exist as a test function, because
//     `go test -run Ghost` exits 0 with "no tests to run".
// What they still cannot prove: that a selected test actually exercises a race
// (only the -race run does) and that the CI timing holds on a cold runner.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const los60ePluginDir = "plugins/opencode-yesmem-v2"

// los60eMakefile holds the parts of a Makefile this test needs: simple variable
// assignments (unexpanded), per-target prerequisites and recipe lines.
type los60eMakefile struct {
	vars    map[string]string
	recipes map[string][]string
	prereqs map[string][]string
}

var (
	los60eAssign     = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*(?::=|\?=|\+=|=)\s*(.*)$`)
	los60eTargetLine = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*::?\s*(.*)$`)
	los60eVarRef     = regexp.MustCompile(`\$\(([A-Za-z_][A-Za-z0-9_]*)\)`)
	los60eTimeout    = regexp.MustCompile(`-timeout[= ]+([0-9]+)(m|s)?`)
	los60eRunFlag    = regexp.MustCompile(`-run[= ]+['"]([^'"]+)['"]`)
)

// los60eParseMakefile reads a Makefile as text, recognising recipe lines by
// their leading tab the way make does. Assignments and target lines are kept
// apart so that a target named "test" is not mistaken for "test := ...".
func los60eParseMakefile(t *testing.T, path string) los60eMakefile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	mf := los60eMakefile{
		vars:    map[string]string{},
		recipes: map[string][]string{},
		prereqs: map[string][]string{},
	}
	target := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "\t") {
			if target != "" {
				mf.recipes[target] = append(mf.recipes[target], strings.TrimSpace(line))
			}
			continue
		}
		line = strings.TrimRight(line, " ")
		if line == "" || strings.HasPrefix(line, "#") {
			target = ""
			continue
		}
		field := strings.TrimPrefix(strings.TrimPrefix(line, "export "), "override ")
		if m := los60eAssign.FindStringSubmatch(field); m != nil {
			mf.vars[m[1]] = strings.TrimSpace(m[2])
			target = ""
			continue
		}
		if m := los60eTargetLine.FindStringSubmatch(line); m != nil {
			target = m[1]
			mf.prereqs[target] = strings.Fields(m[2])
			continue
		}
		target = ""
	}
	return mf
}

// expand resolves $(NAME) for up to five rounds. That is enough for this
// Makefile and avoids a loop that could only add failure modes.
func (mf los60eMakefile) expand(s string) string {
	for i := 0; i < 5; i++ {
		next := los60eVarRef.ReplaceAllStringFunc(s, func(ref string) string {
			name := ref[2 : len(ref)-1]
			if v, ok := mf.vars[name]; ok {
				return v
			}
			return ref
		})
		if next == s {
			break
		}
		s = next
	}
	return s
}

// recipeClosure returns the expanded recipes of a target plus those of its
// prerequisites, so that "test: test-go test-plugin" proves its work even when
// it lives in a sub-target. Depth 3 covers this Makefile and cannot loop.
func (mf los60eMakefile) recipeClosure(target string, depth int) []string {
	out := append([]string{}, mf.recipes[target]...)
	if depth == 0 {
		return out
	}
	for _, p := range mf.prereqs[target] {
		if _, ok := mf.recipes[p]; ok {
			out = append(out, mf.recipeClosure(p, depth-1)...)
		}
	}
	return out
}

func (mf los60eMakefile) linesFor(target string) []string {
	var out []string
	for _, line := range mf.recipeClosure(target, 3) {
		out = append(out, mf.expand(line))
	}
	return out
}

func los60eFindLine(lines []string, want ...string) (string, bool) {
	for _, line := range lines {
		hit := true
		for _, w := range want {
			if !strings.Contains(line, w) {
				hit = false
				break
			}
		}
		if hit {
			return line, true
		}
	}
	return "", false
}

// los60eCodeLines returns the executable lines of a run block: per line the
// comment tail is cut, the way ciCommandLine in the plugin guard does it, so a
// commented-out command is not counted as present.
func los60eCodeLines(run string) []string {
	var out []string
	for _, line := range strings.Split(run, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// los60eMinTimeout requires -timeout of at least minSeconds. Without it a -race
// run hits go test's 600s default and panics on the first timeout, which reads
// like a test failure instead of a limit.
func los60eMinTimeout(t *testing.T, line string, minSeconds int) {
	t.Helper()
	m := los60eTimeout.FindStringSubmatch(line)
	if m == nil {
		t.Errorf("command without -timeout: %q, a -race gate needs a raised limit", line)
		return
	}
	seconds := 0
	for _, c := range m[1] {
		seconds = seconds*10 + int(c-'0')
	}
	if len(m) > 2 && m[2] == "m" {
		seconds *= 60
	}
	if seconds < minSeconds {
		t.Errorf("-timeout %s%s in %q is below %ds", m[1], m[2], line, minSeconds)
	}
}

// los60ePackagesOf collects the package arguments of a go test command.
func los60ePackagesOf(line string) []string {
	var out []string
	for _, f := range strings.Fields(line) {
		if strings.HasPrefix(f, "./") {
			out = append(out, f)
		}
	}
	return out
}

// TestLos60eMakeTestRunsThePluginSuite pins finding 1: `make test` must run bun
// install --frozen-lockfile and bun test in the plugin directory, and neither
// line may swallow its exit status. Without the install the suite aborts on the
// missing devDependency (effect@4.0.1) and reads as a code failure; without
// --frozen-lockfile CI resolves a different dependency set than bun.lock.
//
// It also pins the host fact that made a bare "bun" wrong: on this machine bun
// lives in ~/.bun/bin and is not on PATH for agent shells.
func TestLos60eMakeTestRunsThePluginSuite(t *testing.T) {
	if _, err := os.Stat(los60ePluginDir); err != nil {
		t.Fatalf("%s is missing: %v", los60ePluginDir, err)
	}
	mf := los60eParseMakefile(t, "Makefile")
	lines := mf.linesFor("test")

	installLine, ok := los60eFindLine(lines, los60ePluginDir, "install", "--frozen-lockfile")
	if !ok {
		t.Fatalf("make test does not run 'bun install --frozen-lockfile' in %s, so the plugin suite is not part of the test surface: %q", los60ePluginDir, lines)
	}
	if los60eSwallowsFailure(installLine) {
		t.Errorf("the install line swallows its exit status: %q", installLine)
	}

	var testLine string
	for _, line := range lines {
		if !strings.Contains(line, los60ePluginDir) || strings.Contains(line, "install") {
			continue
		}
		if ciRunsCommand(line, "test") {
			testLine = line
			break
		}
	}
	if testLine == "" {
		t.Fatalf("make test does not run 'bun test' in %s: %q", los60ePluginDir, lines)
	}
	if los60eSwallowsFailure(testLine) {
		t.Errorf("the plugin test line swallows its exit status, so a red suite reports a green make test: %q", testLine)
	}

	bun := mf.vars["BUN"]
	if bun == "" {
		t.Fatalf("Makefile defines no BUN variable: a bare 'bun' is not on PATH here (~/.bun/bin/bun), so make test fails instead of testing")
	}
	if !strings.Contains(bun, "command -v bun") || !strings.Contains(bun, ".bun/bin/bun") {
		t.Errorf("BUN = %q: expected a PATH lookup with the ~/.bun/bin/bun fallback", bun)
	}
	if _, ok := los60eFindLine(mf.recipeClosure("test", 3), "$(BUN)"); !ok {
		t.Errorf("make test never uses $(BUN), so the fallback cannot reach the recipe")
	}
}

// TestLos60eMakeRaceGate pins finding 2 locally: a dedicated target runs -race
// on the race-class packages with a raised timeout and CGO_ENABLED=1 (the
// Makefile exports CGO_ENABLED=0 globally, and the race runtime needs cgo).
// Every go test command in the target is checked on its own, so dropping -race
// from one of them cannot hide behind the other.
func TestLos60eMakeRaceGate(t *testing.T) {
	mf := los60eParseMakefile(t, "Makefile")
	if len(mf.recipes["test-race"]) == 0 {
		t.Fatalf("Makefile has no test-race target, so -race runs nowhere locally")
	}
	lines := mf.linesFor("test-race")

	runPattern := strings.TrimSpace(mf.expand(mf.vars["RACE_RUN"]))
	if runPattern == "" {
		t.Fatalf("RACE_RUN is empty: the gate needs a documented -run selection")
	}
	pkgs := strings.Fields(mf.expand(mf.vars["RACE_PKGS"]))
	if len(pkgs) == 0 {
		t.Fatalf("RACE_PKGS is empty")
	}

	if !strings.Contains(strings.Join(lines, "\n"), "CGO_ENABLED=1") {
		t.Errorf("test-race does not set CGO_ENABLED=1; the Makefile exports CGO_ENABLED=0 and the race runtime needs cgo: %q", lines)
	}

	covered := map[string]bool{}
	commands := 0
	for _, line := range lines {
		if !ciRunsCommand(line, "go test") {
			continue
		}
		if los60eSwallowsFailure(line) {
			t.Errorf("race command swallows its exit status: %q", line)
			continue
		}
		commands++
		if !strings.Contains(line, "-race") {
			t.Errorf("a race command runs without -race: %q", line)
		}
		los60eMinTimeout(t, line, 300)
		if strings.Contains(line, "-run") && !strings.Contains(line, runPattern) {
			t.Errorf("a race command uses a different -run selection than RACE_RUN: %q", line)
		}
		for _, p := range los60ePackagesOf(line) {
			covered[p] = true
		}
	}
	if commands == 0 {
		t.Fatalf("test-race runs no go test command: %q", lines)
	}
	for _, p := range pkgs {
		if !covered[p] {
			t.Errorf("test-race does not cover %s although RACE_PKGS names it: %q", p, lines)
		}
	}
	// Reverse direction: a package added to the recipe only would make the
	// local gate wider than the declared one, and CI would not follow.
	declared := map[string]bool{}
	for _, p := range pkgs {
		declared[p] = true
	}
	for p := range covered {
		if !declared[p] {
			t.Errorf("test-race covers %s, which RACE_PKGS does not name, so local and CI gates differ", p)
		}
	}

	found := false
	for _, p := range mf.prereqs[".PHONY"] {
		if p == "test-race" {
			found = true
		}
	}
	if !found {
		t.Errorf("test-race is missing from .PHONY: %q", mf.prereqs[".PHONY"])
	}
}

// TestLos60eRaceSelectionNamesRealTests closes the silent-empty-gate hole:
// `go test -run Ghost ./pkg` exits 0 with "no tests to run", so a renamed test
// would shrink the gate without a single red signal. Every name in RACE_RUN has
// to exist as a test function inside the covered packages.
func TestLos60eRaceSelectionNamesRealTests(t *testing.T) {
	mf := los60eParseMakefile(t, "Makefile")
	pkgs := strings.Fields(mf.expand(mf.vars["RACE_PKGS"]))
	names := strings.Split(strings.TrimSpace(mf.expand(mf.vars["RACE_RUN"])), "|")
	if len(pkgs) == 0 || len(names) == 0 {
		t.Fatalf("RACE_PKGS/RACE_RUN are empty")
	}

	source := map[string]bool{}
	for _, pkg := range pkgs {
		dir := strings.TrimPrefix(pkg, "./")
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(raw)
			for _, line := range strings.Split(text, "\n") {
				// A commented-out function line must not satisfy the check:
				// renaming a test and leaving "// func TestX(" behind would
				// keep the gate silent while it selects nothing.
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				for _, name := range names {
					if strings.Contains(line, "func "+name+"(") {
						source[name] = true
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	for _, name := range names {
		if !source[name] {
			t.Errorf("RACE_RUN names %q, but no test function with that name exists in %q; the gate would run fewer tests (or none) without a red signal", name, pkgs)
		}
	}
}

func los60eReadWorkflow(t *testing.T) (ciWorkflow, string) {
	t.Helper()
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	return wf, string(raw)
}

// los60eSwallowsFailure extends the plugin guard's matcher with the other ways
// to turn a red command green without removing it: `|| exit 0`, a trailing
// `; true`, and `set +e` in front. The plugin guard only knows `|| true` and
// `|| :`.
func los60eSwallowsFailure(line string) bool {
	if ciSwallowsFailure(line) {
		return true
	}
	if i := strings.Index(line, "#"); i >= 0 {
		line = line[:i]
	}
	for _, pattern := range []string{"|| exit 0", "|| true", "; true", "set +e", "|| :"} {
		if strings.Contains(line, pattern) {
			return true
		}
	}
	return false
}

// los60ePermissionsAreReadOnly accepts only a scope map in which nothing is
// broader than read. `read-all` is deliberately not accepted: it grants read on
// every scope, and this workflow needs contents only.
func los60ePermissionsAreReadOnly(t *testing.T, where string, perms any) {
	t.Helper()
	switch p := perms.(type) {
	case nil:
		t.Errorf("%s has no permissions block, so it runs with the default token scope", where)
	case string:
		t.Errorf("%s permissions = %q; name the scopes explicitly instead of a blanket grant", where, p)
	case map[string]any:
		for scope, level := range p {
			if level != "read" && level != "none" {
				t.Errorf("%s permissions.%s = %v: a workflow that only tests needs no write access", where, scope, level)
			}
		}
	default:
		t.Errorf("%s: unexpected permissions shape %T", where, perms)
	}
}

// TestLos60eCILimitsItsPermissions pins finding 4: the workflow and every job
// set their token scope, and nothing is broader than read. A job that installs
// a foreign cache should not run with the repository default.
func TestLos60eCILimitsItsPermissions(t *testing.T) {
	wf, _ := los60eReadWorkflow(t)
	los60ePermissionsAreReadOnly(t, "ci.yml", wf.Permissions)
	for id, job := range wf.Jobs {
		if job.Permissions != nil {
			los60ePermissionsAreReadOnly(t, "job "+id, job.Permissions)
		}
		// Scoped to jobs that run tests: continue-on-error there is the way to
		// turn a red suite into a green job. An unrelated optional job may use
		// it for its own reasons.
		if !los60eJobRunsTests(job) {
			continue
		}
		if ciContinueOnErrorSet(job.ContinueOnError) {
			t.Errorf("job %q runs tests with continue-on-error, so it can fail silently", id)
		}
		for _, step := range job.Steps {
			if !los60eStepRunsTests(step) {
				continue
			}
			if ciContinueOnErrorSet(step.ContinueOnError) {
				t.Errorf("job %q: test step %q has continue-on-error, so it can fail silently", id, step.Name)
			}
		}
	}
}

// los60eJobRunsTests reports whether any step of the job runs a test command.
func los60eJobRunsTests(job ciWorkflowJob) bool {
	for _, step := range job.Steps {
		if los60eStepRunsTests(step) {
			return true
		}
	}
	return false
}

func los60eStepRunsTests(step ciWorkflowStep) bool {
	for _, line := range los60eCodeLines(step.Run) {
		if strings.Contains(line, "go test") || strings.Contains(line, "bun test") || strings.Contains(line, "make test") {
			return true
		}
	}
	return false
}

// TestLos60eCICarriesARaceGate pins finding 2 in CI and ties the job to the
// Makefile values in both directions: same package set, same -run selection,
// raised -timeout on every race command, CGO_ENABLED=1, no disabled job.
// Locally and in CI running different gates under the same name would make the
// green check worthless.
func TestLos60eCICarriesARaceGate(t *testing.T) {
	mf := los60eParseMakefile(t, "Makefile")
	runPattern := strings.TrimSpace(mf.expand(mf.vars["RACE_RUN"]))
	pkgs := strings.Fields(mf.expand(mf.vars["RACE_PKGS"]))
	if runPattern == "" || len(pkgs) == 0 {
		t.Fatalf("Makefile does not name RACE_RUN/RACE_PKGS, so the drift check has no source")
	}

	wf, _ := los60eReadWorkflow(t)
	commands := 0
	ciCovered := map[string]bool{}
	for id, job := range wf.Jobs {
		if strings.EqualFold(strings.TrimSpace(toString(job.If)), "false") {
			if los60eJobIsRaceJob(job) {
				t.Errorf("job %q runs the race gate but is disabled with if: false", id)
			}
		}
		jobCGO := job.Env["CGO_ENABLED"]
		raceJob := los60eJobIsRaceJob(job)
		for _, step := range job.Steps {
			for _, line := range los60eCodeLines(step.Run) {
				if !strings.Contains(line, "go test") {
					continue
				}
				// Inside the race job every go test command must carry -race;
				// a command that quietly lost the flag would shrink the gate.
				if !strings.Contains(line, "-race") {
					if raceJob {
						t.Errorf("job %q runs the race gate, but this command has no -race: %q", id, line)
					}
					continue
				}
				commands++
				if los60eSwallowsFailure(line) {
					t.Errorf("job %q: race command swallows its exit status: %q", id, line)
					continue
				}
				los60eMinTimeout(t, line, 300)
				if strings.Contains(line, "-run") && !strings.Contains(line, runPattern) {
					t.Errorf("job %q: a filtered -race step does not use the Makefile RACE_RUN selection: %q", id, line)
				}
				cgo := step.Env["CGO_ENABLED"]
				if cgo == "" {
					cgo = jobCGO
				}
				if cgo != "1" {
					t.Errorf("job %q: -race step without CGO_ENABLED=1 (got %q); the race runtime needs cgo", id, cgo)
				}
				for _, p := range los60ePackagesOf(line) {
					ciCovered[p] = true
					ok := false
					for _, want := range pkgs {
						if p == want {
							ok = true
						}
					}
					if !ok {
						t.Errorf("job %q: the -race step covers %s, which RACE_PKGS does not name", id, p)
					}
				}
			}
		}
	}
	if commands == 0 {
		t.Fatalf("no CI job runs 'go test -race', so a returning race would not be caught")
	}
	// A package that the Makefile gate covers but CI does not is exactly the
	// local/CI split this guard exists to prevent.
	for _, p := range pkgs {
		if !ciCovered[p] {
			t.Errorf("the Makefile race gate covers %s, but no CI -race step does", p)
		}
	}

	// Both directions: the CI selection must contain every Makefile name and
	// name nothing extra, otherwise local and CI gates differ under one label.
	runSelections := map[string]bool{}
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			for _, line := range los60eCodeLines(step.Run) {
				if m := los60eRunFlag.FindStringSubmatch(line); m != nil {
					runSelections[m[1]] = true
				}
			}
		}
	}
	if !runSelections[runPattern] {
		t.Errorf("no CI -run uses the Makefile RACE_RUN selection verbatim; found %v", runSelections)
	}
}

// los60eJobIsRaceJob reports whether a job drives the race gate.
func los60eJobIsRaceJob(job ciWorkflowJob) bool {
	for _, step := range job.Steps {
		for _, line := range los60eCodeLines(step.Run) {
			if strings.Contains(line, "go test") && strings.Contains(line, "-race") {
				return true
			}
		}
	}
	return false
}

// toString renders a parsed YAML scalar for comparison. YAML decodes an
// unquoted `if: false` as a bool, so the bool case is the one that matters;
// returning "non-string" for it would leave the check dead for the natural
// spelling.
func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return "non-string"
}
