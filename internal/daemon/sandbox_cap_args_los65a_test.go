package daemon

// YM-206: ai-jail 2.x forwards only the variables it is asked to forward with
// --env; 0.9.x inherited the whole child environment. The cap tool wrapper reads
// its arguments from CAP_ARGS and the caller's session from CAP_SESSION_ID, so
// both have to sit in the sandbox environment on 2.x. Appending them to cmd.Env
// alone is not enough: ai-jail never learns their names and the wrapper dies on
// `JSON.parse(process.env.CAP_ARGS)` with an undefined argument.

import (
	"encoding/json"
	"strings"
	"testing"
)

// Exercise the real cap wrapper through a launcher that, like ai-jail 2.x,
// keeps only explicitly forwarded variables. Merely inspecting sandboxFlags
// would miss adding the inputs to cmd.Env after the flags were already built.
func TestLos65aCapWrapperRunsWithFilteredEnvironment(t *testing.T) {
	if _, err := findBun(); err != nil {
		t.Skip("bun required for the JavaScript cap wrapper")
	}
	los34fPinAiJailScript(t, 2, `#!/bin/bash
forwarded=("PATH=$PATH")
while [ "$#" -gt 0 ]; do
  case "$1" in
    --env) name=$2; forwarded+=("$name=${!name-}"); shift 2 ;;
    --mask|--map|--rw-map|--forward-port|--allow-host|--claude-dir) shift 2 ;;
    --*) shift ;;
    *) break ;;
  esac
done
exec env -i "${forwarded[@]}" "$@"
`)
	isolateHome(t)
	h, _ := mustHandler(t)
	for _, runtime := range []string{"bash", "repl"} {
		t.Run(runtime, func(t *testing.T) {
			body := `printf '%s|%s' "$ARGS_value" "$CAP_SESSION_ID"`
			if runtime == "repl" {
				body = `(args) => args.value + "|" + process.env.CAP_SESSION_ID`
			}
			resp := h.fireCapHandler(CapMeta{Name: "env_probe", Scope: "user", Scripts: []ScriptMeta{{
				Name: "run", Kind: "tool", Runtime: runtime, Sandbox: "standard", Body: body,
			}}}, "", "run", `{"value":"argument arrived"}`, "ses_probe")
			if resp.Error != "" {
				t.Fatal(resp.Error)
			}
			var result struct {
				Output string `json:"output"`
			}
			if err := json.Unmarshal(resp.Result, &result); err != nil {
				t.Fatal(err)
			}
			if strings.Trim(result.Output, "\"\n") != "argument arrived|ses_probe" {
				t.Fatalf("wrapper lost arguments or session: %q", result.Output)
			}
		})
	}
}

func sandboxEnvValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if key, value, ok := strings.Cut(kv, "="); ok && key == name {
			return value, true
		}
	}
	return "", false
}

func TestLos65aCapExecEnvCarriesTheWrapperInputs(t *testing.T) {
	env := capExecSandboxEnv("/run/job.sock", `{"from_version":100}`, "ses_abc")

	if got, ok := sandboxEnvValue(env, "YESMEM_SOCK"); !ok || got != "/run/job.sock" {
		t.Errorf("YESMEM_SOCK = %q (present=%v), want /run/job.sock", got, ok)
	}
	if got, ok := sandboxEnvValue(env, "CAP_ARGS"); !ok || got != `{"from_version":100}` {
		t.Errorf("CAP_ARGS = %q (present=%v): the tool wrapper reads its arguments from it", got, ok)
	}
	if got, ok := sandboxEnvValue(env, "CAP_SESSION_ID"); !ok || got != "ses_abc" {
		t.Errorf("CAP_SESSION_ID = %q (present=%v), want ses_abc", got, ok)
	}
}

func TestLos65aCapExecEnvOmitsAnAbsentSession(t *testing.T) {
	env := capExecSandboxEnv("/run/job.sock", "{}", "")
	if _, ok := sandboxEnvValue(env, "CAP_SESSION_ID"); ok {
		t.Errorf("CAP_SESSION_ID set although the caller has no session: %v", env)
	}
}

func TestLos65aCapExecEnvIsForwardedOnV2(t *testing.T) {
	probed := sandboxVersionProbe
	sandboxVersionProbe = func(string) int { return 2 }
	t.Cleanup(func() { sandboxVersionProbe = probed })

	sb := &Sandbox{
		cfg:        SandboxConfig{Enabled: true, Env: capExecSandboxEnv("/run/job.sock", "{}", "ses_abc")},
		binaryPath: "/usr/local/bin/ai-jail",
	}
	for _, profile := range []SandboxProfile{ProfileStandard, ProfileStrict} {
		flags := sb.sandboxFlags(profile)
		for _, name := range []string{"YESMEM_SOCK", "CAP_ARGS", "CAP_SESSION_ID"} {
			found := false
			for i := 0; i+1 < len(flags); i++ {
				if flags[i] == "--env" && flags[i+1] == name {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: --env %s missing, the sandboxed run would see it as undefined", profile, name)
			}
		}
	}
}
