package capfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-346/OPS-85, key 1 and key 3: a bundled cap that names no sandbox profile
// inherits the configured default, and under ai-jail 2.x that default carries
// --no-network. Every bundled cap would then lose exactly the reach it was
// written for — Telegram over HTTPS, the user journal, the yesmem databases —
// without a single line saying so. So each cap names its profile, and a cap that
// needs egress names the hosts it talks to.
//
// The classification is per cap, not derivable: a repl tool reaches the daemon
// over the mapped socket (standard, no egress), while a cap that reads host state
// the cap sandbox deliberately removes (the journal, the yesmem databases, a
// writable home) has to say none. Reaching for none without that evidence would
// cost the landlock/seccomp isolation for nothing.
//
// Three caps declare none, and that is the honest exception rather than an
// oversight: measured on the live ai-jail 0.9.2 with the daemon's standard flags,
// journalctl --user finds no journal, ~/.claude/yesmem is masked to an empty
// directory, and ~/.local/bin is not writable. Under standard those caps do not
// fail loudly — they return zeros or skip half their work (proxy_health reads a
// muted journal as a healthy system). This does change their profile on 0.9.2,
// from standard to unsandboxed, which is why it is pinned here and reported as a
// decision for the operator instead of slipping in with the network fix.
//
// The declared profile also wins over a job's sandbox=none at run time (the script
// profile has that precedence since YM-230), so the three live bash jobs that run
// these caps keep the profile they run with today.
type los35cCapProfile struct {
	profile string
	hosts   []string
	why     string
}

func los35cBundledProfiles() map[string]los35cCapProfile {
	return map[string]los35cCapProfile{
		"adapter_e2e_test":  {profile: "standard", why: "repl tool, reaches the daemon over the mapped socket, no egress"},
		"cap_collect":       {profile: "standard", why: "repl tool; llm() goes through the yesmem CLI and the daemon, not over the network"},
		"cap_save_analysis": {profile: "standard", why: "repl tool over the daemon socket, no egress"},
		"cap_search":        {profile: "standard", why: "repl tool over the daemon socket, no egress"},
		"cap_delete":        {profile: "none", why: "runs sqlite3 against ~/.claude/yesmem, which the cap sandbox masks as a whole (measured: the DB cannot be opened, the cap then cleans only the cap store)"},
		"deploy":            {profile: "none", why: "installs into ~/.local/bin and restarts the services; the home is a private tmpfs under the standard profile (measured: ~/.local/bin not writable)"},
		"proxy_health":      {profile: "none", why: "reads journalctl --user; the sandbox has no journal dir (measured on 0.9.2 and 2.10.0: 'No journal files were found'), and a muted journal reads as a healthy system"},
		"chrome_headless":   {profile: "none", why: "pre-existing: drives a real browser against arbitrary URLs"},
		"changelog_check":   {profile: "standard", hosts: []string{"raw.githubusercontent.com"}},
		"reddit":            {profile: "standard", hosts: []string{"www.reddit.com", "old.reddit.com"}},
		"rocketchat":        {profile: "standard", hosts: []string{"chat.papoo-service.de"}},
		"telegram":          {profile: "standard", hosts: []string{"api.telegram.org"}},
	}
}

func TestLos35cBundledCapsDeclareProfileAndHosts(t *testing.T) {
	entries, err := os.ReadDir("../../caps/bundled-caps")
	if err != nil {
		t.Fatalf("read bundled caps: %v", err)
	}
	profiles := los35cBundledProfiles()

	seen := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join("../../caps/bundled-caps", entry.Name(), "CAP.md")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		cf, err := Parse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		seen++

		want, ok := profiles[cf.Name]
		if !ok {
			t.Errorf("%s: no classification in los35cBundledProfiles — decide its profile and egress, then add it here", cf.Name)
			continue
		}

		for _, sc := range cf.Scripts {
			if sc.Sandbox != want.profile {
				t.Errorf("%s script %q: sandbox = %q, want %q (%s)", cf.Name, sc.Name, sc.Sandbox, want.profile, want.why)
			}
			if got, expected := strings.Join(sc.AllowedHosts, ","), strings.Join(want.hosts, ","); got != expected {
				t.Errorf("%s script %q: allowed_hosts = %q, want %q", cf.Name, sc.Name, got, expected)
			}
			// allowed_hosts only binds through a sandbox profile (YM-223), and an
			// unsandboxed cap has no egress filter to feed. Both spellings would
			// read like a restriction that is not there.
			if want.profile == "none" && len(sc.AllowedHosts) > 0 {
				t.Errorf("%s script %q: allowed_hosts are inert without a profile, drop them", cf.Name, sc.Name)
			}
		}

		// A declared host has to be a host the script really talks to, otherwise
		// the allowlist is a guess. Escape sequences in regexes are why the text
		// is compared with the backslashes removed.
		text := strings.ReplaceAll(string(raw), `\`, "")
		for _, host := range want.hosts {
			if !strings.Contains(text, host) {
				t.Errorf("%s: declares allowed host %q but never names it", cf.Name, host)
			}
		}
	}

	if seen != len(profiles) {
		t.Errorf("checked %d bundled caps, the classification names %d — keep the table complete", seen, len(profiles))
	}
}
