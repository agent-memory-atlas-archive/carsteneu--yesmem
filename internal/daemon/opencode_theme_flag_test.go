package daemon

import (
	"errors"
	"testing"
)

func TestParseThemeFlagSupport(t *testing.T) {
	tests := []struct {
		name string
		help string
		want bool
	}{
		{
			name: "flag with a type placeholder",
			help: "USAGE\n  opencode <subcommand> [flags] [<directory>]\n\nFLAGS\n" +
				"  --standalone            Run with a private server\n" +
				"  --theme string          Theme to use for this session only (not persisted)\n" +
				"  --auto                  Auto-approve permissions\n",
			want: true,
		},
		{
			name: "flag without a type placeholder",
			help: "FLAGS\n  --theme <name>   Theme to use for this session only\n",
			want: true,
		},
		{
			name: "flag with equals form",
			help: "FLAGS\n  --theme=<name>   Theme to use for this session only\n",
			want: true,
		},
		{
			name: "flag with a short alias",
			help: "FLAGS\n  -t, --theme string   Theme to use for this session only\n",
			want: true,
		},
		{
			name: "binary without the flag",
			help: "FLAGS\n  --standalone            Run with a private server\n  --auto                  Auto-approve permissions\n",
			want: false,
		},
		{
			name: "boolean flag with the same name",
			help: "FLAGS\n  --theme                 Theme to use for this session only\n  --auto                  Auto-approve permissions\n",
			want: false,
		},
		{
			name: "only mentioned in prose",
			help: "FLAGS\n  --standalone   Run with a private server (ignores --theme values)\n",
			want: false,
		},
		{
			name: "only mentioned in a description continuation",
			help: "FLAGS\n  --config string   Config file. Use\n                    --theme to override the theme name\n",
			want: false,
		},
		{
			name: "real help of the binary without the flag",
			help: "FLAGS\n" +
				"  --standalone            Run with a private server instead of the background service\n" +
				"  --server string         Connect to a server URL instead of the background service\n" +
				"  --auto                  Auto-approve permissions that are not explicitly denied\n" +
				"  --continue, -c          Continue the last session\n" +
				"  --session, -s string    Session ID to continue, or to create if it does not exist\n" +
				"  --prompt string         Prompt to use\n" +
				"\nGLOBAL FLAGS\n" +
				"  --help, -h                                                          Show help information\n" +
				"  --completions <bash|zsh|fish|sh>                                    Print shell completion script\n",
			want: false,
		},
		{
			name: "empty help",
			help: "",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseThemeFlagSupport(tt.help); got != tt.want {
				t.Errorf("parseThemeFlagSupport = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpencodeSupportsThemeFlag(t *testing.T) {
	stubHelpExec(t, func(string) (string, error) {
		return "FLAGS\n  --theme string   Theme to use for this session only\n", nil
	})

	if !opencodeSupportsThemeFlag("/usr/bin/opencode") {
		t.Fatal("opencodeSupportsThemeFlag = false, want true for a binary whose help lists --theme")
	}
}

func TestOpencodeSupportsThemeFlagAbsent(t *testing.T) {
	stubHelpExec(t, func(string) (string, error) {
		return "FLAGS\n  --standalone   Run with a private server\n", nil
	})

	if opencodeSupportsThemeFlag("/usr/bin/opencode") {
		t.Fatal("opencodeSupportsThemeFlag = true, want false when the help has no --theme")
	}
}

// TestOpencodeSupportsThemeFlagProbesOncePerBinaryGeneration pins the cache
// contract: the probe is expensive (a process start) and the port swaps the
// binary under the daemon, so the identity has to be mtime+size, not the path.
func TestOpencodeSupportsThemeFlagProbesOncePerBinaryGeneration(t *testing.T) {
	probes := 0
	stubHelpExec(t, func(string) (string, error) {
		probes++
		return "FLAGS\n  --theme string   Theme to use for this session only\n", nil
	})
	identity := "mtime=1000,size=42"
	stubBinaryIdentity(t, func(string) (string, bool) { return identity, true })

	if !opencodeSupportsThemeFlag("/usr/bin/opencode") {
		t.Fatal("first probe = false, want true")
	}
	if !opencodeSupportsThemeFlag("/usr/bin/opencode") {
		t.Fatal("cached probe = false, want true")
	}
	if probes != 1 {
		t.Fatalf("help executions = %d, want 1 (second call must be served from the cache)", probes)
	}

	identity = "mtime=2000,size=99" // the port replaced the binary
	if !opencodeSupportsThemeFlag("/usr/bin/opencode") {
		t.Fatal("probe after binary swap = false, want true")
	}
	if probes != 2 {
		t.Fatalf("help executions after binary swap = %d, want 2", probes)
	}
}

// TestOpencodeSupportsThemeFlagWithoutBinaryIdentity: an unreadable binary is
// never probed — a failed stat must not be cached, or a temporarily missing
// binary would look permanently incompatible.
func TestOpencodeSupportsThemeFlagWithoutBinaryIdentity(t *testing.T) {
	probes := 0
	stubHelpExec(t, func(string) (string, error) {
		probes++
		return "FLAGS\n  --theme string   Theme\n", nil
	})
	stubBinaryIdentity(t, func(string) (string, bool) { return "", false })

	if opencodeSupportsThemeFlag("/usr/bin/opencode") {
		t.Fatal("opencodeSupportsThemeFlag = true, want false when the binary cannot be stat'ed")
	}
	if probes != 0 {
		t.Fatalf("help executions = %d, want 0 for an unreadable binary", probes)
	}
}

// TestOpencodeSupportsThemeFlagHelpFailure: a binary that cannot be executed
// (or that hangs and gets killed) must degrade to "no flag" instead of failing
// the spawn.
func TestOpencodeSupportsThemeFlagHelpFailure(t *testing.T) {
	stubHelpExec(t, func(string) (string, error) {
		return "", errors.New("fork/exec: permission denied")
	})
	stubBinaryIdentity(t, func(string) (string, bool) { return "mtime=1,size=1", true })

	if opencodeSupportsThemeFlag("/usr/bin/opencode") {
		t.Fatal("opencodeSupportsThemeFlag = true, want false when --help fails")
	}
}
