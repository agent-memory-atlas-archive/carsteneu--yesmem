package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-334 Befund 3: claudeConfigFlags rendert --map nach dem Maskenfilter, der auf
// cfg.ReadOnlyMounts schon gelaufen ist. Der Pfad kommt damit als Mount zurueck in
// die Sandbox, obwohl die Maskenliste ihn verdeckt (ai-jail legt Mounts nach den
// Masken, gemessen an 0.9.2). Der Befund greift, sobald masked_paths aus der
// Konfiguration kommt.
func TestLos34hClaudeConfigMapRespectsMask(t *testing.T) {
	home := t.TempDir()
	claudeJSON := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", claudeJSON, err)
	}
	work := t.TempDir()

	cases := []struct {
		name string
		mask string
	}{
		{"the config itself is masked", claudeJSON},
		{"a parent of the config is masked", home},
		{"the config is masked with a trailing separator", claudeJSON + string(filepath.Separator)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			los34cPinAiJail(t, 0)
			sb := NewSandbox(SandboxConfig{
				Enabled:      true,
				WorkDir:      work,
				ClaudeConfig: claudeJSON,
				MaskedPaths:  []string{tc.mask},
			})
			if !sb.Available() {
				t.Fatal("the stand-in ai-jail is not resolvable")
			}
			flags := strings.Join(sb.sandboxFlags(ProfileStandard), " ")
			if strings.Contains(flags, "--map "+claudeJSON) {
				t.Errorf("a masked path is mapped read-only again, flags = %q", flags)
			}
			if !strings.Contains(flags, "--mask "+tc.mask) {
				t.Errorf("the mask itself is missing, flags = %q", flags)
			}
		})
	}
}

// YM-334 Befund 4: ein gedroppter Mount war nur eine Logzeile. Liegt die
// yesmem-CLI oder der Job-Socket je unter einer Maske, laufen store() und llm()
// im Cap in ein Kommando, das es im Sandbox nicht gibt, und der Cap-Aufrufer
// sieht nur den Folgesymptom-Fehler. AddReadOnlyMounts muss den Verlust melden.
// Gleichheit zaehlt wie "darunter": ein Mount, der genau die Maske ist, legt
// denselben Pfad wieder frei (Review-Befund N3).
func TestLos34hAddReadOnlyMountsReportsDropped(t *testing.T) {
	t.Run("below a masked directory", func(t *testing.T) {
		masked := t.TempDir()
		cli := filepath.Join(masked, "bin", "yesmem")
		curl := filepath.Join(t.TempDir(), "curl")

		sb := NewSandbox(SandboxConfig{Enabled: true, MaskedPaths: []string{masked}})
		dropped := sb.AddReadOnlyMounts(cli, curl)

		if len(dropped) != 1 || dropped[0] != cli {
			t.Errorf("dropped = %v, want [%s]", dropped, cli)
		}
		if len(sb.cfg.ReadOnlyMounts) != 1 || sb.cfg.ReadOnlyMounts[0] != curl {
			t.Errorf("kept mounts = %v, want [%s]", sb.cfg.ReadOnlyMounts, curl)
		}
	})

	t.Run("exactly the mask", func(t *testing.T) {
		masked := filepath.Join(t.TempDir(), ".claude.json")
		sb := NewSandbox(SandboxConfig{Enabled: true, MaskedPaths: []string{masked}})
		dropped := sb.AddReadOnlyMounts(masked)

		if len(dropped) != 1 || dropped[0] != masked {
			t.Errorf("dropped = %v, want [%s]", dropped, masked)
		}
		if len(sb.cfg.ReadOnlyMounts) != 0 {
			t.Errorf("kept mounts = %v, want none", sb.cfg.ReadOnlyMounts)
		}
	})

	// A mask spelled with a trailing separator names the same path; a raw string
	// compare misses it while the "below" branch cleans on its own (review F2).
	t.Run("mask with a trailing separator", func(t *testing.T) {
		masked := t.TempDir()
		sb := NewSandbox(SandboxConfig{Enabled: true, MaskedPaths: []string{masked + string(filepath.Separator)}})
		dropped := sb.AddReadOnlyMounts(masked)

		if len(dropped) != 1 || dropped[0] != masked {
			t.Errorf("dropped = %v, want [%s]", dropped, masked)
		}
	})
}

// requiredMountErr ist die Entscheidung, die der Cap-Ausfuehrungspfad daraus
// macht: ein erforderlicher Mount, den der Maskenfilter entfernt hat, verweigert
// den Lauf. Die Meldung nennt keinen Host-Pfad (Befund 5) und traegt
// errMaskPrepare, damit der Lauf nicht als Skriptfehler gilt. Nur das Standard-
// Profil rendert Mounts: none startet das Kommando direkt, strict verwirft jedes
// --map unter --lockdown, dort aendert ein verlorener Mount nichts (N1).
func TestLos34hRequiredMountLossRefusesTheRun(t *testing.T) {
	sock := "/run/yesmem-run/job.sock"
	cli := "/home/alice/.local/bin/yesmem"

	err := requiredMountErr([]string{cli}, []string{cli, sock}, ProfileStandard)
	if err == nil {
		t.Fatal("a lost yesmem CLI mount must refuse the run")
	}
	if !errors.Is(err, errMaskPrepare) {
		t.Errorf("error %q does not carry errMaskPrepare", err)
	}
	if strings.Contains(err.Error(), "/home/alice") {
		t.Errorf("refusal names a host path: %v", err)
	}

	for _, profile := range []SandboxProfile{ProfileNone, ProfileStrict} {
		if err := requiredMountErr([]string{cli}, []string{cli, sock}, profile); err != nil {
			t.Errorf("profile %s renders no mount, a lost one must not refuse it: %v", profile, err)
		}
	}

	if err := requiredMountErr([]string{"/opt/bin/curl"}, []string{cli, sock}, ProfileStandard); err != nil {
		t.Errorf("an optional mount loss must not refuse the run: %v", err)
	}
	if err := requiredMountErr(nil, []string{cli, sock}, ProfileStandard); err != nil {
		t.Errorf("no loss at all must not refuse the run: %v", err)
	}
}

// YM-334 Befund 5: die Masken-Ablehnung nennt absolute Host-Pfade und geht so an
// den Cap-Aufrufer. Der Text fuer den Aufrufer muss den Pfad verlieren, der
// vollstaendige Fehler bleibt im Daemon-Log.
func TestLos34hMaskRefusalHidesHostPaths(t *testing.T) {
	raw := fmt.Errorf("%w: mask placeholder /home/alice/.claude/settings.json: permission denied", errMaskPrepare)
	got := maskRefusalForCaller(raw)
	if strings.Contains(got, "/home/alice") {
		t.Errorf("caller-facing text names a host path: %q", got)
	}
	if got == "" {
		t.Error("caller-facing text is empty")
	}
	if plain := maskRefusalForCaller(errors.New("boom")); plain != "boom" {
		t.Errorf("a non-mask error must pass through, got %q", plain)
	}
}

// Dieselbe Grenze an der Verdrahtung: execute_cap muss den Text durch
// maskRefusalForCaller schicken. Ein Masken-Platzhalter unter procfs laesst sich
// nicht anlegen und traegt darum einen absoluten Host-Pfad in PrepareErr; der
// Aufrufer darf ihn nicht sehen (Review-Befund N4).
func TestLos34hExecuteCapMaskRefusalHidesHostPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	h, _ := mustHandler(t)
	los34cPinAiJail(t, 0)
	// A temporary HOME keeps the default masks out of the real home; the failing
	// placeholder is the caps dir below, which lives under procfs.
	isolateHomeAt(t, t.TempDir())
	h.capsDir = "/proc/self/los34h-caps"
	los35bInsertCap(t, h, "los34h_mask_probe", "", "echo ok", "standard")

	resp := h.handleExecuteCap(map[string]any{"name": "los34h_mask_probe"})
	if resp.Error == "" {
		t.Fatal("a mask that cannot be applied must refuse the run")
	}
	if strings.Contains(resp.Error, "/proc/self") {
		t.Errorf("caller-facing text names a host path: %q", resp.Error)
	}
	if !strings.Contains(resp.Error, "sandbox mask") {
		t.Errorf("refusal does not name the mask: %q", resp.Error)
	}
}

// YM-334 Befund 6, verbleibender Teil: save_cap (YM-230) und der Job-Lauf
// (YM-346) verweigern den ungesandboxten Lauf einer projektgebundenen Cap, der
// Ausfuehrungspfad execute_cap tat es nicht. Eine Zeile aus der Zeit vor den
// Sperren laeuft damit voellig ungemaskt.
func TestLos34hExecuteCapRefusesProjectBoundWithoutProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	h, _ := mustHandler(t)
	isolateHomeAt(t, t.TempDir())
	los35bUnsandboxed(t, h)
	// The caller states its project the way a real call does (session or cwd); a
	// project-bound cap is not reachable without one (YM-359).
	los35bInsertCap(t, h, "los34h_probe", "/home/tester/memory/yesmem", "echo ok", "")

	resp := h.handleExecuteCap(map[string]any{"name": "los34h_probe", "project": "/home/tester/memory/yesmem"})
	if resp.Error == "" {
		t.Fatalf("a project-bound cap without a sandbox line ran unsandboxed, result = %s", resp.Result)
	}
	if !strings.Contains(resp.Error, "unsandboxed") {
		t.Errorf("refusal does not name the reason: %q", resp.Error)
	}
}

// Dieselbe Zeile ohne Projektbindung bleibt erlaubt: sandbox=none ist fuer eine
// user-scoped Cap eine Entscheidung, nicht die Luecke aus Befund 6.
func TestLos34hExecuteCapAllowsUserScopedWithoutProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	h, _ := mustHandler(t)
	isolateHomeAt(t, t.TempDir())
	los35bUnsandboxed(t, h)
	los34hInsertUserCap(t, h, "los34h_user_probe", "echo ok")

	resp := h.handleExecuteCap(map[string]any{"name": "los34h_user_probe"})
	if resp.Error != "" {
		t.Fatalf("a user-scoped cap may run unsandboxed: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "ok") {
		t.Errorf("result = %s", resp.Result)
	}
}

// Die vier Bindungsformen, die die gemeinsame Regel unterscheidet (Review-Befund
// N2): eine Zeile mit Projekt oder mit scope=project ist gebunden und darf nicht
// ungesandboxt laufen, eine user-scoped Zeile ohne Projekt darf es. Die dritte
// Form entsteht heute nur noch als Altzeile, save_cap leert das Projekt bei
// scope=user.
func TestLos34hExecuteCapBindingShapes(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	cases := []struct {
		name       string
		project    string
		scope      string
		wantRefuse bool
	}{
		{"project and project scope", "/home/tester/memory/yesmem", "project", true},
		{"project scope without a project", "", "project", true},
		{"legacy project column on a user-scoped row", "/home/tester/memory/yesmem", "user", true},
		{"user scope without a project", "", "user", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := mustHandler(t)
			isolateHomeAt(t, t.TempDir())
			los35bUnsandboxed(t, h)
			insertCapRow(t, h, "los34h_shape", tc.project, "echo ok", "", tc.scope)

			resp := h.handleExecuteCap(map[string]any{"name": "los34h_shape"})
			if tc.wantRefuse {
				if resp.Error == "" {
					t.Fatalf("the row ran unsandboxed, result = %s", resp.Result)
				}
				return
			}
			if resp.Error != "" {
				t.Fatalf("a user-scoped row may run unsandboxed: %s", resp.Error)
			}
		})
	}
}

// YM-334 Befund 4 am zweiten Einstiegspunkt (Review-Befund 3): die Mounts eines
// geplanten Jobs werden in newSandbox gesetzt. Eine Maske ueber den Job-Socket
// muss den Standardlauf verweigern statt nur zu loggen, den ungesandboxten Lauf
// aber nicht anfassen.
func TestLos34hJobSandboxRefusesLostSocketMount(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	h, _ := mustHandler(t)
	isolateHomeAt(t, t.TempDir())
	los34cPinAiJail(t, 0)
	base := t.TempDir()
	h.dataDir = filepath.Join(base, "data")
	sock := JobSocketPath(h.dataDir)
	// A trailing separator marks the mask as a directory; without it the
	// placeholder would be laid down as a file exactly where the runtime dir
	// belongs.
	runtimeDir := filepath.Dir(sock) + string(filepath.Separator)

	sb := h.newSandbox(SandboxConfig{Enabled: true, MaskedPaths: []string{runtimeDir}})
	if sb.cfg.PrepareErr == nil {
		t.Fatal("a mask over the job socket must be refused")
	}
	if _, _, err := sb.RunWithProfile("echo ok", 5, ProfileStandard); !errors.Is(err, errMaskPrepare) {
		t.Errorf("standard run should be refused with errMaskPrepare, got %v", err)
	}
	if out, code, err := sb.RunWithProfile("echo ok", 5, ProfileNone); err != nil || code != 0 || !strings.Contains(out, "ok") {
		t.Errorf("the unsandboxed profile renders no mount and must run: out=%q code=%d err=%v", out, code, err)
	}
}

// los34hInsertUserCap seeds one user-scoped cap row: the negative case of the
// execution-path lock, which los35bInsertCap cannot express (it always writes
// scope project).
func los34hInsertUserCap(t *testing.T, h *Handler, name, body string) {
	t.Helper()
	insertCapRow(t, h, name, "", body, "", "user")
}
