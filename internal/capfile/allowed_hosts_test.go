package capfile

import (
	"strings"
	"testing"
)

// D13b: the standard sandbox profile's egress allowlist belongs to the script,
// not to the daemon — a self-hosted service cannot be hardcoded in Go.
func TestParse_ScriptAllowedHosts(t *testing.T) {
	src := "---\nname: x\ndescription: \"x\"\n---\n\n## Purpose\nx\n\n## Scripts\n\n### x\nkind: handler\nsandbox: standard\nallowed_hosts: api.telegram.org, chat.example.org\n\n```bash\necho hi\n```\n"

	cf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := cf.Scripts[0].AllowedHosts
	if len(got) != 2 || got[0] != "api.telegram.org" || got[1] != "chat.example.org" {
		t.Errorf("AllowedHosts = %v, want [api.telegram.org chat.example.org]", got)
	}
}

func TestParse_ScriptAllowedHostsOmittedIsEmpty(t *testing.T) {
	src := "---\nname: x\ndescription: \"x\"\n---\n\n## Purpose\nx\n\n## Scripts\n\n### x\nkind: handler\n\n```bash\necho hi\n```\n"

	cf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cf.Scripts[0].AllowedHosts) != 0 {
		t.Errorf("AllowedHosts = %v, want empty (no egress)", cf.Scripts[0].AllowedHosts)
	}
}

// A URL is the natural mistake; ai-jail's --allow-host takes a bare hostname.
func TestParse_ScriptAllowedHostRejectsURL(t *testing.T) {
	src := "---\nname: x\ndescription: \"x\"\n---\n\n## Purpose\nx\n\n## Scripts\n\n### x\nkind: handler\nallowed_hosts: https://api.telegram.org/bot\n\n```bash\necho hi\n```\n"

	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected a rejection for a URL in allowed_hosts")
	}
	if !strings.Contains(err.Error(), "allowed host") {
		t.Errorf("error should name the allowed host, got %v", err)
	}
}

// The field has to survive the disk round-trip: the daemon writes caps back out
// and re-parses them on the next sync.
func TestWrite_RoundTripsAllowedHosts(t *testing.T) {
	cf := &CapFile{
		Name:        "my_cap",
		Description: "egress cap",
		Version:     1,
		Scope:       "user",
		Scripts: []Script{{
			Name:         "fetch",
			Kind:         "handler",
			Runtime:      "bash",
			Sandbox:      "standard",
			AllowedHosts: []string{"api.telegram.org", "chat.example.org"},
			Body:         "echo hi",
		}},
	}

	out := string(Render(cf))
	if !strings.Contains(out, "allowed_hosts: api.telegram.org, chat.example.org") {
		t.Fatalf("allowed_hosts missing from the rendered CAP.md:\n%s", out)
	}

	back, err := Parse([]byte(out))
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	got := back.Scripts[0].AllowedHosts
	if len(got) != 2 || got[0] != "api.telegram.org" || got[1] != "chat.example.org" {
		t.Errorf("round-trip lost the allowlist: %v", got)
	}
}

func TestSplitHostList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a.org", []string{"a.org"}},
		{" a.org , b.org ", []string{"a.org", "b.org"}},
		{", ,", nil},
	}
	for _, tc := range cases {
		got := SplitHostList(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("SplitHostList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("SplitHostList(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}

func TestValidateHost(t *testing.T) {
	for _, ok := range []string{"api.telegram.org", "127.0.0.1"} {
		if err := ValidateHost(ok); err != nil {
			t.Errorf("ValidateHost(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "https://x.org", "x.org:443", "x.org/path", "x y"} {
		if err := ValidateHost(bad); err == nil {
			t.Errorf("ValidateHost(%q) = nil, want an error", bad)
		}
	}
}
