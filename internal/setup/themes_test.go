package setup

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/carsteneu/yesmem/plugins"
)

// bundledThemeNames is the theme family shipped with yesmem: a neutral
// default, one per-hue-tinted dark theme per agent type, and a light theme.
var bundledThemeNames = []string{
	"yesmem-default",
	"yesmem-dark",
	"yesmem-loop",
	"yesmem-research",
	"yesmem-subresearch",
	"yesmem-light",
}

// agentThemeNames are the four dark agent themes: shared text/border
// structure, deliberately per-hue-tinted backgrounds, one accent per type.
var agentThemeNames = []string{
	"yesmem-dark",
	"yesmem-loop",
	"yesmem-research",
	"yesmem-subresearch",
}

// opencodeThemeSlots mirrors the slot set opencode's bundled themes define
// (packages/tui/src/theme/assets/*.json). The port's v1 migration resolves
// every slot it is given, so a theme that omits one is only accidentally
// usable — the contract is that we cover all of them.
var opencodeThemeSlots = []string{
	"accent", "background", "backgroundElement", "backgroundPanel", "border", "borderActive",
	"borderSubtle", "diffAdded", "diffAddedBg", "diffAddedLineNumberBg", "diffContext",
	"diffContextBg", "diffHighlightAdded", "diffHighlightRemoved", "diffHunkHeader",
	"diffLineNumber", "diffRemoved", "diffRemovedBg", "diffRemovedLineNumberBg", "error", "info",
	"markdownBlockQuote", "markdownCode", "markdownCodeBlock", "markdownEmph", "markdownHeading",
	"markdownHorizontalRule", "markdownImage", "markdownImageText", "markdownLink",
	"markdownLinkText", "markdownListEnumeration", "markdownListItem", "markdownStrong",
	"markdownText", "primary", "secondary", "success", "syntaxComment", "syntaxFunction",
	"syntaxKeyword", "syntaxNumber", "syntaxOperator", "syntaxPunctuation", "syntaxString",
	"syntaxType", "syntaxVariable", "text", "textMuted", "warning",
}

// themeFile is the subset of the V1 theme document the tests inspect.
type themeFile struct {
	Defs  map[string]string `json:"defs"`
	Theme map[string]any    `json:"theme"`
}

func readBundledTheme(t *testing.T, name string) themeFile {
	t.Helper()
	data, err := plugins.AgentThemes.ReadFile("themes/" + name + ".json")
	if err != nil {
		t.Fatalf("read embedded theme %s: %v", name, err)
	}
	var parsed themeFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse embedded theme %s: %v", name, err)
	}
	return parsed
}

// TestBundledThemesCoverEverySlot guards the contract with opencode's theme
// migration: every slot opencode resolves must be present, and every slot must
// point at a hex colour so the theme stays dark-only (a {dark,light} variant
// pair would make opencode derive two modes).
func TestBundledThemesCoverEverySlot(t *testing.T) {
	for _, name := range bundledThemeNames {
		theme := readBundledTheme(t, name)
		for _, slot := range opencodeThemeSlots {
			value, ok := theme.Theme[slot]
			if !ok {
				t.Errorf("%s: slot %q missing", name, slot)
				continue
			}
			ref, ok := value.(string)
			if !ok {
				t.Errorf("%s: slot %q is not a string (%T) — a {dark,light} pair would split the theme", name, slot, value)
				continue
			}
			hex, ok := theme.Defs[ref]
			if !ok {
				t.Errorf("%s: slot %q references unknown def %q", name, slot, ref)
				continue
			}
			if !isHexColor(hex) {
				t.Errorf("%s: slot %q resolves to %q, want #rrggbb", name, slot, hex)
			}
		}
	}
}

// TestBundledThemesReadableContrast enforces the legibility targets the theme
// family was built against: body text at >= 7:1 on the background, accents at
// >= 4.5:1, and borders bright enough to be visible at all.
func TestBundledThemesReadableContrast(t *testing.T) {
	type check struct {
		slot     string
		minimum  float64
		slotName string
	}
	checks := []check{
		{slot: "text", minimum: 7, slotName: "text/background"},
		{slot: "textMuted", minimum: 7, slotName: "textMuted/background"},
		{slot: "primary", minimum: 4.5, slotName: "primary/background"},
		{slot: "accent", minimum: 4.5, slotName: "accent/background"},
		{slot: "secondary", minimum: 4.5, slotName: "secondary/background"},
		{slot: "syntaxKeyword", minimum: 4.5, slotName: "syntaxKeyword/background"},
		{slot: "markdownHeading", minimum: 4.5, slotName: "markdownHeading/background"},
		{slot: "success", minimum: 4.5, slotName: "success/background"},
		{slot: "error", minimum: 4.5, slotName: "error/background"},
		{slot: "warning", minimum: 4.5, slotName: "warning/background"},
		{slot: "border", minimum: 1.5, slotName: "border/background"},
		{slot: "borderActive", minimum: 2, slotName: "borderActive/background"},
	}
	for _, name := range bundledThemeNames {
		theme := readBundledTheme(t, name)
		background := theme.Defs[theme.Theme["background"].(string)]
		for _, c := range checks {
			foreground := theme.Defs[theme.Theme[c.slot].(string)]
			if got := contrastRatio(foreground, background); got < c.minimum {
				t.Errorf("%s: %s = %.2f:1, want >= %.1f:1 (%s on %s)",
					name, c.slotName, got, c.minimum, foreground, background)
			}
		}
	}
}

// TestAgentThemesShareStructureAndTintBackgrounds states the family contract:
// the four agent themes share text and border slots, their backgrounds are
// deliberately tinted per hue (the tint is what makes agent types recognisable
// at a glance), and each keeps its own accent. The default stays neutral with
// pure-white text; the light theme inverts the luminance direction.
func TestAgentThemesShareStructureAndTintBackgrounds(t *testing.T) {
	shared := []string{"text", "textMuted", "border", "borderActive"}
	reference := readBundledTheme(t, agentThemeNames[0])

	for _, name := range agentThemeNames {
		theme := readBundledTheme(t, name)
		for _, slot := range shared {
			got := theme.Defs[theme.Theme[slot].(string)]
			want := reference.Defs[reference.Theme[slot].(string)]
			if got != want {
				t.Errorf("%s: structure slot %q = %s, want the shared value %s", name, slot, got, want)
			}
		}
	}

	backgrounds := map[string]string{}
	accents := map[string]string{}
	for _, name := range agentThemeNames {
		theme := readBundledTheme(t, name)
		backgrounds[name] = theme.Defs[theme.Theme["background"].(string)]
		accents[name] = theme.Defs[theme.Theme["accent"].(string)]
	}
	for slot, values := range map[string]map[string]string{"background": backgrounds, "accent": accents} {
		seen := map[string]string{}
		for name, value := range values {
			if other, dup := seen[value]; dup {
				t.Errorf("%s and %s share the %s %s — per-agent %s is the point", other, name, slot, value, slot)
			}
			seen[value] = name
		}
	}

	deflt := readBundledTheme(t, "yesmem-default")
	defbg := deflt.Defs[deflt.Theme["background"].(string)]
	lr, _ := strconv.ParseUint(defbg[1:3], 16, 8)
	lg, _ := strconv.ParseUint(defbg[3:5], 16, 8)
	lb, _ := strconv.ParseUint(defbg[5:7], 16, 8)
	if lr != lg || lg != lb {
		t.Errorf("yesmem-default background %s is tinted, want neutral", defbg)
	}
	if text := deflt.Defs[deflt.Theme["text"].(string)]; text != "#ffffff" {
		t.Errorf("yesmem-default text = %s, want pure white", text)
	}

	light := readBundledTheme(t, "yesmem-light")
	lightbg := light.Defs[light.Theme["background"].(string)]
	lighttext := light.Defs[light.Theme["text"].(string)]
	if luminance(lightbg) < 0.8 {
		t.Errorf("yesmem-light background %s is not light", lightbg)
	}
	if luminance(lighttext) > 0.05 {
		t.Errorf("yesmem-light text %s is not dark", lighttext)
	}
}

func TestInstallThemesInstallsFamily(t *testing.T) {
	home := themesHome(t)

	n, err := InstallThemes(home)
	if err != nil {
		t.Fatalf("InstallThemes: %v", err)
	}
	if n != len(bundledThemeNames) {
		t.Fatalf("InstallThemes wrote %d files, want %d", n, len(bundledThemeNames))
	}

	for _, name := range bundledThemeNames {
		dst := filepath.Join(home, ".config", "opencode", "themes", name+".json")
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read installed theme %s: %v", name, err)
		}
		want, err := plugins.AgentThemes.ReadFile("themes/" + name + ".json")
		if err != nil {
			t.Fatalf("read embedded theme %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s: installed content differs from the embedded source", name)
		}
	}
}

func TestInstallThemesIdempotentAndUpdating(t *testing.T) {
	home := themesHome(t)

	if n, err := InstallThemes(home); err != nil || n != len(bundledThemeNames) {
		t.Fatalf("first install: n=%d err=%v, want %d/nil", n, err, len(bundledThemeNames))
	}
	if n, err := InstallThemes(home); err != nil || n != 0 {
		t.Fatalf("second install: n=%d err=%v, want 0/nil", n, err)
	}

	stale := filepath.Join(home, ".config", "opencode", "themes", "yesmem-loop.json")
	if err := os.WriteFile(stale, []byte(`{"theme":{"primary":"#000000"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if n, err := InstallThemes(home); err != nil || n != 1 {
		t.Fatalf("install after edit: n=%d err=%v, want 1/nil", n, err)
	}
	got, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf("read refreshed theme: %v", err)
	}
	want, _ := plugins.AgentThemes.ReadFile("themes/yesmem-loop.json")
	if string(got) != string(want) {
		t.Error("edited theme was not restored to the embedded content")
	}
}

// TestInstallThemesSkipsWithoutOpencodeConfig keeps Claude-only installs clean:
// no ~/.config/opencode means no themes directory is created.
func TestInstallThemesSkipsWithoutOpencodeConfig(t *testing.T) {
	home := t.TempDir()

	n, err := InstallThemes(home)
	if err != nil || n != 0 {
		t.Fatalf("InstallThemes without opencode config: n=%d err=%v, want 0/nil", n, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "opencode")); !os.IsNotExist(err) {
		t.Errorf("themes install created %s, want nothing", filepath.Join(home, ".config", "opencode"))
	}
}

// themesHome prepares a fake HOME with an existing ~/.config/opencode dir, the
// prerequisite for the opencode integration (and therefore for themes).
func themesHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0755); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestRemoveThemesLeavesUserThemes: uninstall removes what yesmem installed,
// not the themes a user added to the same directory.
func TestRemoveThemesLeavesUserThemes(t *testing.T) {
	home := themesHome(t)
	if n, err := InstallThemes(home); err != nil || n != len(bundledThemeNames) {
		t.Fatalf("InstallThemes: n=%d err=%v", n, err)
	}
	own := filepath.Join(home, ".config", "opencode", "themes", "my-own.json")
	if err := os.WriteFile(own, []byte(`{"theme":{"primary":"#000000"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RemoveThemes(home); err != nil {
		t.Fatalf("RemoveThemes: %v", err)
	}
	for _, name := range bundledThemeNames {
		dst := filepath.Join(home, ".config", "opencode", "themes", name+".json")
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("bundled theme %s still present after uninstall", name)
		}
	}
	if _, err := os.Stat(own); err != nil {
		t.Errorf("user theme was removed: %v", err)
	}
}

// TestRemoveThemesWithoutInstall: uninstall on a machine that never had the
// themes installed is a no-op, not an error.
func TestRemoveThemesWithoutInstall(t *testing.T) {
	home := t.TempDir()
	if err := RemoveThemes(home); err != nil {
		t.Fatalf("RemoveThemes without an install: %v", err)
	}
}

// TestRemoveThemesReportsFailure pins that a blocked removal surfaces as an
// error: removeOpencodePlugin turns it into a warning instead of aborting the
// rest of the uninstall.
func TestRemoveThemesReportsFailure(t *testing.T) {
	home := themesHome(t)
	blocked := filepath.Join(home, ".config", "opencode", "themes", bundledThemeNames[0]+".json")
	if err := os.MkdirAll(filepath.Join(blocked, "keep"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveThemes(home); err == nil {
		t.Fatal("expected an error when a theme path cannot be removed")
	}
}

func isHexColor(value string) bool {
	if len(value) != 7 || value[0] != '#' {
		return false
	}
	for _, r := range value[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// contrastRatio is the WCAG 2.x relative-luminance ratio between two #rrggbb
// colours.
func contrastRatio(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func luminance(hex string) float64 {
	channel := func(i int) float64 {
		value, _ := strconv.ParseUint(hex[1+i*2:3+i*2], 16, 8)
		c := float64(value) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(0) + 0.7152*channel(1) + 0.0722*channel(2)
}
