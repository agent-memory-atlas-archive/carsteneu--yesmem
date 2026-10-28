package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ciPluginTestDir ist das Verzeichnis, dessen bun-Suite in der CI laufen muss.
// Die Referenz im Workflow wird gegen das Dateisystem geprueft, damit ein
// Verzeichnis-Rename keinen toten Job hinterlaesst.
const ciPluginTestDir = "plugins/opencode-yesmem-v2"

type ciWorkflowStep struct {
	Name             string         `yaml:"name"`
	Uses             string         `yaml:"uses"`
	Run              string         `yaml:"run"`
	WorkingDirectory string         `yaml:"working-directory"`
	With             map[string]any `yaml:"with"`
	ContinueOnError  any            `yaml:"continue-on-error"`
	Env              map[string]string `yaml:"env"`
	If               any            `yaml:"if"`
}

type ciWorkflowJob struct {
	Steps            []ciWorkflowStep  `yaml:"steps"`
	Env              map[string]string `yaml:"env"`
	Permissions      any               `yaml:"permissions"`
	ContinueOnError  any               `yaml:"continue-on-error"`
	If               any               `yaml:"if"`
}

type ciWorkflow struct {
	Jobs        map[string]ciWorkflowJob `yaml:"jobs"`
	Permissions any                      `yaml:"permissions"`
}

// TestCIPluginSuiteLos59d haelt YM-430 fest: die bun-Suite des opencode-v2-
// Plugins ist die einzige Absicherung dieses Codes, und sie lief bis dahin in
// keiner CI. Der Test ist ein Struktur-Scan (er parst den Workflow, er faehrt
// bun nicht) und prueft, was den Job erst wirksam macht:
//   - setup-bun vor install und test, mit gepinnter bun-version, sonst liegt
//     bun nicht im PATH und die gemessene Version ist nicht die laufende,
//   - bun install vor bun test im Plugin-Verzeichnis: effect@4.0.1 ist
//     devDependency, ohne Install bricht der Lauf nach zehn roten Dateien ab
//     und 81 von 167 Tests laufen gar nicht erst,
//   - --frozen-lockfile auf genau dem Install-Befehl: bun.lock ist eingecheckt,
//     CI darf keine andere Abhaengigkeitsmenge aufloesen,
//   - ein Test-Step, der fehlschlagen kann: continue-on-error, "|| true" und
//     ein Namensfilter machen aus dem Job eine gruene Nullnummer.
func TestCIPluginSuiteLos59d(t *testing.T) {
	if _, err := os.Stat(ciPluginTestDir); err != nil {
		t.Fatalf("%s fehlt: %v", ciPluginTestDir, err)
	}

	raw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("ci.yml lesen: %v", err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("ci.yml parsen: %v", err)
	}

	jobs := 0
	for id, job := range wf.Jobs {
		testIdx := -1
		for i, step := range job.Steps {
			if ciRunsCommand(step.Run, "bun test") && ciStepInPluginDir(step) {
				testIdx = i
				break
			}
		}
		if testIdx < 0 {
			continue
		}
		jobs++

		setupIdx, installIdx := -1, -1
		for i := 0; i < testIdx; i++ {
			step := job.Steps[i]
			if strings.Contains(step.Uses, "setup-bun") {
				setupIdx = i
			}
			if ciRunsCommand(step.Run, "bun install") && ciStepInPluginDir(step) {
				installIdx = i
			}
		}
		if setupIdx < 0 {
			t.Errorf("Job %q: kein Step mit uses: oven-sh/setup-bun@v2 vor bun test", id)
		} else {
			if v, ok := job.Steps[setupIdx].With["bun-version"]; !ok || strings.TrimSpace(fmt.Sprint(v)) == "" {
				t.Errorf("Job %q: setup-bun ohne bun-version, die Suite ist auf 1.4.2 gemessen", id)
			}
			if installIdx >= 0 && setupIdx > installIdx {
				t.Errorf("Job %q: setup-bun (Step %d) steht nach bun install (Step %d)", id, setupIdx, installIdx)
			}
		}
		if installIdx < 0 {
			t.Errorf("Job %q: kein 'bun install' in %s vor bun test", id, ciPluginTestDir)
		} else if line, _ := ciCommandLine(job.Steps[installIdx].Run, "bun install"); !strings.Contains(line, "--frozen-lockfile") {
			t.Errorf("Job %q: der Install-Befehl %q ist nicht frozen, CI darf bun.lock nicht neu aufloesen", id, strings.TrimSpace(line))
		}

		if ciContinueOnErrorSet(job.Steps[testIdx].ContinueOnError) {
			t.Errorf("Job %q: der Test-Step laeuft mit continue-on-error und kann nicht fehlschlagen", id)
		}
		if ciSwallowsFailure(job.Steps[testIdx].Run) {
			t.Errorf("Job %q: der Test-Befehl schluckt seinen Exit-Status (%q)", id, strings.TrimSpace(job.Steps[testIdx].Run))
		}
	}
	if jobs == 0 {
		t.Fatalf("kein CI-Job faehrt 'bun test' in %s", ciPluginTestDir)
	}
}

// ciCommandLine liefert die erste Codezeile des run-Blocks, die cmd aufruft.
// Kommentarzeilen zaehlen nicht, sonst erfuellt ein auskommentierter Schritt
// den Test und ein Kommentar mit "--frozen-lockfile" die Flag-Pruefung.
func ciCommandLine(run, cmd string) (string, bool) {
	for _, line := range strings.Split(run, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if strings.Contains(line, cmd) {
			return line, true
		}
	}
	return "", false
}

func ciRunsCommand(run, cmd string) bool {
	_, ok := ciCommandLine(run, cmd)
	return ok
}

// ciStepInPluginDir akzeptiert beide ueblichen Formen, das Verzeichnis zu
// waehlen: working-directory am Step oder ein cd im selben Befehl.
func ciStepInPluginDir(step ciWorkflowStep) bool {
	dir := strings.TrimSuffix(strings.TrimPrefix(step.WorkingDirectory, "./"), "/")
	if dir == ciPluginTestDir {
		return true
	}
	_, ok := ciCommandLine(step.Run, "cd "+ciPluginTestDir)
	return ok
}

// ciContinueOnErrorSet liest das Feld, weil YAML es als bool oder als String
// liefern kann; jeder gesetzte Wert ausser false gilt als gesetzt.
func ciContinueOnErrorSet(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "true")
	}
	return true
}

// ciSwallowsFailure erkennt die zwei Wege, aus einem roten Testlauf einen
// gruenen Job zu machen, ohne den Step zu entfernen.
func ciSwallowsFailure(run string) bool {
	for _, line := range strings.Split(run, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if strings.Contains(line, "|| true") || strings.Contains(line, "|| :") {
			return true
		}
		if strings.Contains(line, "bun test") && (strings.Contains(line, " -t ") || strings.Contains(line, "--filter")) {
			return true
		}
	}
	return false
}
