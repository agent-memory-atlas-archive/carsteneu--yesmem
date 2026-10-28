package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/carsteneu/yesmem/internal/capfile"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// parseScriptsParam decodes the `scripts` param of save_cap into validated
// ScriptMeta values, applying defaults and provider-form conversion for REPL
// bodies so storage holds the canonical adapter-decoupled form.
func parseScriptsParam(s string) ([]ScriptMeta, error) {
	if s == "" {
		return nil, fmt.Errorf("scripts is required (JSON array)")
	}
	var raw []ScriptMeta
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("scripts: invalid JSON: %w", err)
	}
	return normalizeScripts(raw)
}

func scriptsFromSaveCapParams(name string, params map[string]any) ([]ScriptMeta, error) {
	if scriptsJSON := stringOr(params, "scripts", ""); scriptsJSON != "" {
		return parseScriptsParam(scriptsJSON)
	}

	handlerBash := stringOr(params, "handler_bash", "")
	handlerREPL := stringOr(params, "handler_repl", "")
	if handlerBash == "" && handlerREPL == "" {
		return nil, fmt.Errorf("'scripts' or at least one legacy handler ('handler_bash' or 'handler_repl') is required")
	}

	schema := stringOr(params, "schema", "")
	var scripts []ScriptMeta
	if handlerREPL != "" {
		scripts = append(scripts, ScriptMeta{
			Name:    name,
			Kind:    "tool",
			Runtime: "repl",
			Lang:    "javascript",
			Body:    handlerREPL,
			Schema:  schema,
		})
	}
	if handlerBash != "" {
		scriptName := name
		kind := "tool"
		scriptSchema := schema
		if handlerREPL != "" {
			scriptName = name + "_bash"
			kind = "handler"
			scriptSchema = ""
		} else if scriptSchema == "" {
			scriptSchema = "{}"
		}
		scripts = append(scripts, ScriptMeta{
			Name:    scriptName,
			Kind:    kind,
			Runtime: "bash",
			Lang:    "bash",
			Body:    handlerBash,
			Schema:  scriptSchema,
		})
	}
	return normalizeScripts(scripts)
}

func normalizeScripts(raw []ScriptMeta) ([]ScriptMeta, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("scripts: at least one script required")
	}
	seen := map[string]bool{}
	for i := range raw {
		sc := &raw[i]
		if sc.Name == "" {
			return nil, fmt.Errorf("scripts[%d]: name is required", i)
		}
		// The name is the fn execute_cap takes and the identifier the catalogs
		// print; a name outside the alphabet cannot be both.
		if err := capfile.ValidateScriptName(sc.Name); err != nil {
			return nil, fmt.Errorf("scripts[%d]: %w", i, err)
		}
		if seen[sc.Name] {
			return nil, fmt.Errorf("scripts: duplicate name %q", sc.Name)
		}
		seen[sc.Name] = true

		if sc.Kind == "" {
			sc.Kind = "tool"
		}
		if sc.Kind != "tool" && sc.Kind != "handler" {
			return nil, fmt.Errorf("script %q: kind must be tool or handler, got %q", sc.Name, sc.Kind)
		}

		if sc.Runtime == "" {
			sc.Runtime = "repl"
		}
		if sc.Runtime != "repl" && sc.Runtime != "bash" {
			return nil, fmt.Errorf("script %q: runtime must be repl or bash, got %q", sc.Name, sc.Runtime)
		}

		if sc.Body == "" {
			return nil, fmt.Errorf("script %q: body is required", sc.Name)
		}

		if sc.Kind == "tool" && sc.Schema == "" {
			if sc.Runtime == "repl" {
				sc.Schema = capfile.DeriveSchema(sc.Body)
			} else {
				return nil, fmt.Errorf("script %q: tool with runtime=bash requires explicit schema", sc.Name)
			}
		}
		if sc.Kind == "handler" && sc.Schema != "" {
			return nil, fmt.Errorf("script %q: handlers cannot have a schema", sc.Name)
		}

		for i, host := range sc.AllowedHosts {
			trimmed := strings.TrimSpace(host)
			if err := capfile.ValidateHost(trimmed); err != nil {
				return nil, fmt.Errorf("script %q: %w", sc.Name, err)
			}
			sc.AllowedHosts[i] = trimmed
		}

		if sc.Runtime == "repl" {
			sc.Body = capfile.GenericToProvider(sc.Body, capfile.DefaultAdapters())
		}
	}
	return raw, nil
}

// capIsProjectBound is the binding rule the sandbox locks share: a row is bound
// when it carries a project, or when it claims project scope. save_cap refuses to
// store the second shape without a project, so it survives only in rows written
// before that check. save_cap, the job path and execute_cap each computed this
// themselves until YM-334; the edges had already drifted (a scope=project row with
// an empty project column was refused by the job path and allowed by execute_cap).
func capIsProjectBound(project string, meta CapMeta) bool {
	return project != "" || meta.Scope == "project"
}

// validateScriptSandboxForScope holds save_cap to the same lock the CAP.md
// parser enforces: a cap bound to a project may not run a script unsandboxed.
//
// The check runs on the EFFECTIVE profile, not on the raw string: an omitted
// sandbox line inherits the configured default, and with
// default_sandbox_profile=none that default is none just like a written-out
// none (YM-230). It also rejects a profile that cannot be parsed, because the
// lock cannot vouch for a value it cannot resolve.
//
// lockApplies is the caller's answer to "does this save leave the cap bound to a
// project". The lock is keyed on that binding, not on the raw scope param:
// save_cap stores an omitted scope as project-bound whenever a project comes
// along, so keying on the param left the live path (gcm_log_audit) open.
//
// metadataOnlySameBinding marks the one exemption — a save that neither adds nor
// changes a script and leaves the binding where it was. It exists so a metadata
// edit (description, tags, test date) of a cap which already is project-bound
// with an unsandboxed script does not fail; it is not the way into that state,
// and it does not repair one: a repair is a save whose scripts name a profile,
// which is exactly what the lock demands. The caller decides the exemption,
// because only the caller knows the stored project.
func (h *Handler) validateScriptSandboxForScope(lockApplies, metadataOnlySameBinding bool, scripts []ScriptMeta) error {
	if !lockApplies {
		return nil
	}
	for _, sc := range scripts {
		profile, err := h.resolveScriptSandbox(sc.Sandbox)
		if err != nil {
			return fmt.Errorf("script %q: %w", sc.Name, err)
		}
		if profile != ProfileNone {
			continue
		}
		if metadataOnlySameBinding {
			log.Printf("save_cap: metadata-only update keeps an unsandboxed script %q on a project-bound cap", sc.Name)
			return nil
		}
		if sc.Sandbox == "" {
			return fmt.Errorf("script %q: no sandbox line, and the configured default_sandbox_profile is none, which is not allowed on a project-bound cap (declare a profile or use scope=user)", sc.Name)
		}
		return fmt.Errorf("script %q: sandbox=none not allowed on a project-bound cap (use scope=user)", sc.Name)
	}
	return nil
}

// detectRequiresFromScriptMetas scans every script body for adapter primitive
// calls and returns the set of detected primitive names.
func detectRequiresFromScriptMetas(scripts []ScriptMeta) []string {
	primitives := []string{"file", "store", "web"}
	seen := map[string]bool{}
	var out []string
	for _, sc := range scripts {
		for _, p := range primitives {
			if seen[p] {
				continue
			}
			if scriptCallsPrimitive(sc, p) {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// bashStoreCall matches the cap_store primitive as a shell command word: preceded
// by a line start or a command separator, followed by whitespace, a quote or a
// substitution. The bash adapter installs store() as a shell function, so the
// parenthesis form never appears in a bash body — a plain `store(` search found
// nothing there. A bare word at the end of a line is not a call: `command -v
// store` probes for the function, it does not invoke it.
var bashStoreCall = regexp.MustCompile(`(?:^|[ \t;&|(){])store(?:[ \t]|["'$])`)

// bashStoreProbe matches the shapes that ask whether the function exists instead
// of running it. They are cut out of the line before the call pattern runs, so a
// capability check does not declare the adapter.
var bashStoreProbe = regexp.MustCompile(`(?:command[ \t]+-[vV]|type|which)[ \t]+store\b`)

// scriptCallsPrimitive reports whether one script body calls the adapter
// primitive. A repl script calls it as a function; a bash script calls the one
// primitive its adapter offers (store) as a command.
func scriptCallsPrimitive(sc ScriptMeta, name string) bool {
	if sc.Runtime != "bash" {
		return strings.Contains(sc.Body, name+"(")
	}
	if name != "store" {
		return false
	}
	for _, line := range strings.Split(sc.Body, "\n") {
		line = bashStoreProbe.ReplaceAllString(stripShellComment(line), "")
		if bashStoreCall.MatchString(line) {
			return true
		}
	}
	return false
}

// stripShellComment cuts a trailing shell comment: the first '#' that starts a
// word outside quotes. A '#' inside quotes is data (a printf format, a URL
// fragment) and stays. Without this, prose after code ("echo ok # store the
// result") declared an adapter — and because a declared list wins on the next
// save, the false positive became permanent.
func stripShellComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t' || line[i-1] == ';'):
			return line[:i]
		}
	}
	return line
}

// capRequires returns the adapter declarations for a cap: an explicit list from
// the caller wins (capfile.Parse treats it the same way, and it is the author's
// contract), otherwise they are detected from the script bodies.
func capRequires(params map[string]any, scripts []ScriptMeta) []string {
	if declared := parseRequiresParam(params); len(declared) > 0 {
		return declared
	}
	return detectRequiresFromScriptMetas(scripts)
}

// capScriptsEqual reports whether two script sets are identical in every field a
// CAP.md round-trip carries. An absent and an empty list are the same thing.
func capScriptsEqual(a, b []ScriptMeta) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		switch {
		case a[i].Name != b[i].Name,
			a[i].Kind != b[i].Kind,
			a[i].Runtime != b[i].Runtime,
			a[i].Lang != b[i].Lang,
			// A CAP.md round-trip trims the trailing newlines of a body
			// (capfile.extractFirstCodeBlock). Comparing the raw strings would
			// report the daemon's own export as a different body on every start.
			strings.TrimRight(a[i].Body, "\n") != strings.TrimRight(b[i].Body, "\n"),
			a[i].Schema != b[i].Schema,
			a[i].Sandbox != b[i].Sandbox,
			a[i].Timeout != b[i].Timeout,
			!capHostsEqual(a[i].AllowedHosts, b[i].AllowedHosts):
			return false
		}
	}
	return true
}

// scriptDigest identifies a script set. It is the row's memory of what its source
// said, see CapMeta.ImportedDigest.
func scriptDigest(scripts []ScriptMeta) string {
	raw, err := json.Marshal(scripts)
	if err != nil {
		return ""
	}
	return textutil.ContentHash(string(raw))
}

// capRowRepairedSinceImport reports whether the row holds more than the body it came
// from. The auto-correct pass and every save without a source revision edit the row in
// place and leave ImportedDigest where it was, so a digest that no longer matches the
// row IS the repair. Such a row is the later state: an import at the same revision
// does not overwrite it, and an export of it belongs on the disk. A row stored before
// the field existed carries no evidence either way and answers no.
func capRowRepairedSinceImport(meta CapMeta) bool {
	return meta.ImportedDigest != "" && meta.ImportedDigest != scriptDigest(meta.Scripts)
}

// capUnchanged reports whether a save would leave the stored cap exactly as it is.
// It answers the import gate's real question — is there anything to change — over
// the scripts and the metadata CapMeta carries. The free-text sections a CAP.md
// also has (Purpose, Database) are not part of CapMeta and are never imported by
// CapFileToParams, so a file that changes only those reads as unchanged.
// Version and ImportedVersion stay out (they move independently of the content),
// and so does TestDate (stamped per save, so it would report a change every time).
func capUnchanged(old CapMeta, oldProject string, next CapMeta, nextProject string) bool {
	return capScriptsEqual(old.Scripts, next.Scripts) &&
		old.Description == next.Description &&
		strings.Join(old.Requires, ",") == strings.Join(next.Requires, ",") &&
		strings.Join(old.Tags, ",") == strings.Join(next.Tags, ",") &&
		old.Tested == next.Tested &&
		old.AutoActive == next.AutoActive &&
		capScope(old.Scope, oldProject) == capScope(next.Scope, nextProject) &&
		capActionsEqual(old.Actions, next.Actions)
}

// capScope folds the two spellings of a user-scoped cap into one: no scope with no
// project and an explicit scope=user with no project bind the cap the same way, so
// a file saying "user" over a row that says nothing is not a change. The emptiness is
// tested on the project itself, so the answer does not depend on how a project value
// is canonicalized.
func capScope(scope, project string) string {
	if project == "" {
		return "user"
	}
	if scope == "" {
		return "project"
	}
	return scope
}

func capActionsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func capHostsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// scriptToJSFunc renders a ScriptMeta as a JS function string suitable for
// passing to registerTool. REPL scripts are returned as-is (they are already
// async function expressions). Bash scripts are wrapped so the generated
// JS function shells out via the sh() adapter primitive.
func scriptToJSFunc(sc ScriptMeta) string {
	if sc.Runtime == "bash" {
		return fmt.Sprintf("async () => sh(%q)", sc.Body)
	}
	return sc.Body
}

// mergeScripts merges incoming scripts into existing scripts by name.
// Scripts in existing that have a matching name in incoming are replaced;
// unmatched existing scripts are kept; new names in incoming are appended.
// When incoming is empty the caller's upstream validation (normalizeScripts)
// rejects it before we are reached; the nil-return is defensive.
func mergeScripts(existing, incoming []ScriptMeta) []ScriptMeta {
	if len(incoming) == 0 {
		return nil
	}
	byName := make(map[string]int)
	for i, sc := range existing {
		byName[sc.Name] = i
	}
	merged := make([]ScriptMeta, len(existing))
	copy(merged, existing)
	for _, sc := range incoming {
		if idx, ok := byName[sc.Name]; ok {
			merged[idx] = sc
		} else {
			merged = append(merged, sc)
		}
	}
	return merged
}
