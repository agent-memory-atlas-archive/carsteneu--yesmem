package daemon

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/extraction"
)

// constructionSiteFile is the one file in this package allowed to build a raw
// extraction.LLMClient. Every daemon LLM path has to go through
// newDaemonLLMClient there, so that no path can silently miss the redaction
// wrapper, the budget metering, the api gate or the per-call cap.
const constructionSiteFile = "llm_client_factory.go"

func daemonSourceDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate the package source directory")
	}
	return filepath.Dir(thisFile)
}

func readDaemonSource(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(daemonSourceDir(t), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// extractionClientConstructors are the exported ways to build a provider-facing
// client. newDaemonLLMClient is the daemon's only allowed entry point into them.
var extractionClientConstructors = map[string]string{
	"NewLLMClient":    "the daemon builds clients through a helper that returns a raw one",
	"NewCLIClient":    "a provider client built without the daemon's decoration",
	"NewClient":       "a provider client built without the daemon's decoration",
	"NewOpenAIClient": "a provider client built without the daemon's decoration",
}

// packageFiles lists every non-test Go file of this package, subdirectories
// included, keyed by path relative to the package directory.
func packageFiles(t *testing.T) map[string]string {
	t.Helper()
	dir := daemonSourceDir(t)
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("walk package dir: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("found 0 source files — the package directory lookup is broken")
	}
	return files
}

func parseDaemonFile(t *testing.T, name, src string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file
}

// TestDaemonLLMConstructionSitesAreCentralised parses every non-test source file
// of this package and requires that no provider client is constructed outside
// llm_client_factory.go.
//
// It resolves the extraction import per file, so an aliased import
// (`ext.NewLLMClient`) is caught too, and it checks every exported constructor,
// not just NewLLMClient — a path that builds a CLIClient directly would bypass
// the redaction wrapper just as effectively. A dot import cannot be resolved to
// a selector at all, so it is reported instead of silently skipped.
//
// The companion behavioral tests pin the factory itself:
// TestNewDaemonLLMClient_BuildsARealClient proves it is not a stub, and the
// decoration tests prove what it wraps.
func TestDaemonLLMConstructionSitesAreCentralised(t *testing.T) {
	var offenders []string
	for name, src := range packageFiles(t) {
		if name == constructionSiteFile {
			continue
		}
		file := parseDaemonFile(t, name, src)
		local, dotImport := extractionLocalName(file)
		if dotImport {
			offenders = append(offenders, name+": dot-imports the extraction package, which hides every constructor call from this guard")
			continue
		}
		if local == "" {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != local {
				return true
			}
			if why, bad := extractionClientConstructors[sel.Sel.Name]; bad {
				offenders = append(offenders, fmt.Sprintf("%s: %s.%s (%s)", name, local, sel.Sel.Name, why))
			}
			return true
		})
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("%s must be the only file constructing provider clients; also found:\n  %s\nroute those sites through newDaemonLLMClient",
			constructionSiteFile, strings.Join(offenders, "\n  "))
	}
}

// extractionLocalName returns the identifier the file uses for the extraction
// package (its alias when one is set), or "" when the file does not import it.
// dotImport reports a dot import, which no selector walk can follow.
func extractionLocalName(file *ast.File) (name string, dotImport bool) {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "github.com/carsteneu/yesmem/internal/extraction" {
			continue
		}
		if imp.Name == nil {
			return "extraction", false
		}
		if imp.Name.Name == "." {
			return "", true
		}
		return imp.Name.Name, false
	}
	return "", false
}

// TestDaemonLLMConstructionSitesDeclareTheirLayers pins which budget/gate/cap
// layers every construction site declares.
//
// The sites live inside Run's startup goroutine and cannot be driven from a
// test, so this reads the call sites out of the AST: a site either takes its
// layers from meteredOpts (the fully decorated paths) or names them in an
// llmClientOpts literal, and the resulting signature per site has to match the
// inventory in llm_client_factory.go. Switching a site to a bare literal, or
// dropping a layer from one, changes its signature and fails here — a comment
// cannot satisfy this check.
func TestDaemonLLMConstructionSitesDeclareTheirLayers(t *testing.T) {
	var got []string
	for name, src := range packageFiles(t) {
		file := parseDaemonFile(t, name, src)
		base, added := localOptsSignatures(file)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "newDaemonLLMClient" {
				return true
			}
			if len(call.Args) != 2 {
				got = append(got, fmt.Sprintf("%s: UNEXPECTED ARITY %d", name, len(call.Args)))
				return true
			}
			got = append(got, name+": "+siteSignature(call.Args[1], base, added))
			return true
		})
	}
	sort.Strings(got)

	want := []string{
		"daemon.go: literal[]",                    // summarize (Haiku) base client
		"daemon.go: literal[gate]",                // briefing refinement
		"daemon.go: literal[throttle,tracker]",    // two-pass summarize
		"daemon.go: literal[throttle,tracker]",    // two-pass extract
		"daemon.go: meteredOpts(extract)",         // extraction client
		"daemon.go: meteredOpts(extract)",         // llm_complete override
		"daemon.go: meteredOpts(extract)",         // bash-job auto-correct
		"daemon.go: meteredOpts(quality)+tracker", // quality client
		"gap_review.go: literal[tracker]",         // gap review
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the daemon's LLM construction sites no longer declare the layers from the inventory in llm_client_factory.go.\n got:\n  %s\n want:\n  %s\nIf a change is intentional, update the inventory AND this list.",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// siteSignature renders the decoration one construction site declares, resolving
// an opts argument that was assigned to a local first (qualityOpts, o) and the
// layer overrides applied to it afterwards.
func siteSignature(arg ast.Expr, base map[string]string, added map[string][]string) string {
	if id, ok := arg.(*ast.Ident); ok {
		b, found := base[id.Name]
		if !found {
			return "UNRESOLVED LOCAL " + id.Name
		}
		sig := b
		for _, key := range added[id.Name] {
			sig += "+" + key
		}
		return sig
	}
	return optsSignature(arg)
}

// localOptsSignatures records, per local variable, the decoration signature of
// its initialiser and the layer fields assigned to it later.
func localOptsSignatures(file *ast.File) (base map[string]string, added map[string][]string) {
	base = map[string]string{}
	added = map[string][]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		switch lhs := assign.Lhs[0].(type) {
		case *ast.Ident:
			base[lhs.Name] = optsSignature(assign.Rhs[0])
		case *ast.SelectorExpr:
			id, ok := lhs.X.(*ast.Ident)
			if !ok || !isLayerField(lhs.Sel.Name) {
				return true
			}
			added[id.Name] = append(added[id.Name], lhs.Sel.Name)
		}
		return true
	})
	for name := range added {
		sort.Strings(added[name])
	}
	return base, added
}

func isLayerField(name string) bool {
	switch name {
	case "tracker", "gate", "throttle", "maxPerCall":
		return true
	}
	return false
}

// optsSignature renders the decoration a construction site declares: the layer
// keys of its llmClientOpts literal, or the meteredOpts bucket when it takes the
// layers from the shared env.
func optsSignature(arg ast.Expr) string {
	switch a := arg.(type) {
	case *ast.CallExpr:
		fn, ok := a.Fun.(*ast.Ident)
		if !ok || fn.Name != "meteredOpts" {
			return "UNKNOWN CALL"
		}
		if len(a.Args) != 3 {
			return "meteredOpts UNEXPECTED ARITY"
		}
		lit, ok := a.Args[2].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "meteredOpts with a non-literal bucket"
		}
		bucket, err := strconv.Unquote(lit.Value)
		if err != nil {
			return "meteredOpts with an unquotable bucket"
		}
		return "meteredOpts(" + bucket + ")"
	case *ast.CompositeLit:
		var layers []string
		for _, elt := range a.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "tracker", "gate", "throttle", "maxPerCall":
				layers = append(layers, key.Name)
			}
		}
		sort.Strings(layers)
		return "literal[" + strings.Join(layers, ",") + "]"
	}
	return "UNKNOWN OPTS"
}

// TestDaemonLLMEnvIsWiredToTheSharedHandles pins the argument side of the env:
// the one call has to hand over the daemon's live tracker and gate, not nil. A
// nil there would leave every metered path unmetered while
// TestMeteredOpts_CarriesTheSharedDecoration still passes.
func TestDaemonLLMEnvIsWiredToTheSharedHandles(t *testing.T) {
	file := parseDaemonFile(t, "daemon.go", readDaemonSource(t, "daemon.go"))

	var calls []*ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "daemonLLMEnv" {
			calls = append(calls, call)
		}
		return true
	})
	if len(calls) != 1 {
		t.Fatalf("expected exactly one daemonLLMEnv call in daemon.go, found %d", len(calls))
	}
	call := calls[0]
	if len(call.Args) != 7 {
		t.Fatalf("daemonLLMEnv takes 7 arguments, the call passes %d", len(call.Args))
	}
	for _, idx := range []int{3, 4} { // tracker, gate
		ident, ok := call.Args[idx].(*ast.Ident)
		if !ok || ident.Name == "nil" {
			t.Fatalf("daemonLLMEnv argument %d must be the daemon's live handle, got %s", idx, exprText(call.Args[idx]))
		}
	}
	if maxPerCall, ok := call.Args[6].(*ast.Ident); ok && maxPerCall.Name == "nil" {
		t.Fatal("daemonLLMEnv must be given the configured per-call cap, got nil")
	}
}

func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.BasicLit:
		return v.Value
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	}
	return fmt.Sprintf("%T", e)
}

// daemonLLMEnv is where the three metered paths get their handles, so this is
// the test that holds them in place.
func TestDaemonLLMEnv_CarriesTheSharedHandles(t *testing.T) {
	tracker := extraction.NewBudgetTracker(5)
	gate := extraction.NewAPIGate(tracker, tracker, 5, 0.50)
	throttled := false
	env := daemonLLMEnv("key", "/bin/false", "http://example.invalid", tracker, gate, func() bool { return throttled }, 1.25)

	if env.apiKey != "key" || env.claudeBinary != "/bin/false" || env.baseURL != "http://example.invalid" {
		t.Errorf("provider wiring not carried: %+v", env)
	}
	if env.tracker != tracker {
		t.Error("the shared tracker is not carried")
	}
	if env.gate != gate {
		t.Error("the api gate is not carried")
	}
	if env.maxPerCall != 1.25 {
		t.Errorf("the per-call cap is not carried: %v", env.maxPerCall)
	}
	if env.throttle == nil || env.throttle() != throttled {
		t.Error("the throttle function is not carried")
	}
}

func TestMeteredOpts_CarriesTheSharedHandles(t *testing.T) {
	tracker := extraction.NewBudgetTracker(5)
	gate := extraction.NewAPIGate(tracker, tracker, 5, 0.50)
	env := daemonLLMEnv("key", "/bin/false", "http://example.invalid", tracker, gate, nil, 1.25)

	o := meteredOpts(env, "test-model", "extract")
	if o.model != "test-model" || o.gateBucket != "extract" {
		t.Errorf("model/bucket not carried: model=%q bucket=%q", o.model, o.gateBucket)
	}
	if o.tracker != tracker {
		t.Error("the shared tracker is not carried")
	}
	if o.gate != gate {
		t.Error("the api gate is not carried")
	}
	if o.maxPerCall != 1.25 {
		t.Errorf("the per-call cap is not carried: %v", o.maxPerCall)
	}
	if o.apiKey != "key" || o.baseURL != "http://example.invalid" {
		t.Errorf("provider wiring not carried: apiKey=%q baseURL=%q", o.apiKey, o.baseURL)
	}
	if o.provider != "" {
		t.Errorf("meteredOpts must leave the provider to the caller, got %q", o.provider)
	}
}

// TestHandlerLLMOverrideFactoryIsWiredInTheDaemon guards the one construction
// path that happens per call instead of at startup: llm_complete builds a client
// on the fly when a caller asks for a different model. If the daemon stops
// wiring that factory, the override silently falls back to an undecorated
// client again — the regression this test exists for.
//
// The check reads the AST rather than the file text: it looks for the function
// literal that becomes the override factory — the right-hand side of an
// assignment to handler.llmOverrideClient (the old field wiring) or an argument
// of handler.SetLLMRouting (the guarded wiring) — and requires it to call
// newDaemonLLMClient, so a comment naming it does not satisfy the test.
func TestHandlerLLMOverrideFactoryIsWiredInTheDaemon(t *testing.T) {
	file := parseDaemonFile(t, "daemon.go", readDaemonSource(t, "daemon.go"))

	wired := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn := overrideFactoryLiteral(n)
		if fn == nil {
			return true
		}
		ast.Inspect(fn, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "newDaemonLLMClient" {
				wired = true
			}
			return true
		})
		return true
	})

	if !wired {
		t.Fatal("daemon.go must wire the llm_complete override factory to a function that calls newDaemonLLMClient — otherwise an llm_complete model override escapes the budget, the api gate and the per-call cap")
	}
}

// overrideFactoryLiteral returns the function literal a node publishes as the
// llm_complete override factory, or nil when the node is not that wiring.
func overrideFactoryLiteral(n ast.Node) *ast.FuncLit {
	switch node := n.(type) {
	case *ast.AssignStmt:
		if len(node.Lhs) != 1 || len(node.Rhs) != 1 {
			return nil
		}
		sel, ok := node.Lhs[0].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "llmOverrideClient" {
			return nil
		}
		fn, _ := node.Rhs[0].(*ast.FuncLit)
		return fn
	case *ast.CallExpr:
		sel, ok := node.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetLLMRouting" {
			return nil
		}
		for _, arg := range node.Args {
			if fn, ok := arg.(*ast.FuncLit); ok {
				return fn
			}
		}
	}
	return nil
}

// The decoration tests below run against decorateDaemonLLMClient. This one pins
// its caller: newDaemonLLMClient has to really build a provider client, so a
// factory that quietly returned a stub (or the redactor alone) cannot keep the
// construction-site guard green.
func TestNewDaemonLLMClient_BuildsARealClient(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' '{\"result\":\"ok\"}'\n"), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	ac := &config.Config{}
	client, err := newDaemonLLMClient(ac, llmClientOpts{provider: "cli", model: "test-model", claudeBinary: bin})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	if client == nil {
		t.Fatal("no client built")
	}
	if client.Name() != "cli" || client.Model() != "test-model" {
		t.Fatalf("expected the real cli client, got name=%q model=%q", client.Name(), client.Model())
	}
	if out, err := client.Complete("sys", "user"); err != nil || out != "ok" {
		t.Fatalf("client does not complete: out=%q err=%v", out, err)
	}
}

// An unknown provider has to surface as an error rather than a nil client the
// caller treats as "LLM disabled".
func TestNewDaemonLLMClient_UnknownProviderErrors(t *testing.T) {
	ac := &config.Config{}
	if _, err := newDaemonLLMClient(ac, llmClientOpts{provider: "definitely-not-a-provider", model: "m"}); err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
}

// The gap review used to build its own client and passed on both things its
// siblings apply: the redaction, and the daily budget it spends from. It now goes
// through decorateDaemonLLMClient like every other path.
func TestDecorateDaemonLLMClient_RedactsAndMeters(t *testing.T) {
	ac := &config.Config{}
	ac.SecretsSanitization.Enabled = true

	secret := "AKIAIOSFODNN7EXAMPLE"
	var seen string
	spy := &spySummarizeClient{
		completeFunc: func(system, userMsg string, opts ...extraction.CallOption) (string, error) {
			seen = userMsg
			return "[]", nil
		},
	}

	tracker := extraction.NewBudgetTracker(1.00)
	client := decorateDaemonLLMClient(spy, ac, llmClientOpts{tracker: tracker})
	if _, err := client.Complete("sys", secret); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if strings.Contains(seen, secret) {
		t.Errorf("the prompt reached the model unredacted: %q", seen)
	}

	// The decorator has to meter against the tracker it was handed: a second
	// tracker on the same bucket would enforce the cap against a private snapshot.
	if _, calls := tracker.SpentToday(); calls != 1 {
		t.Errorf("the decorated client must meter against the shared tracker, got %d calls", calls)
	}
}

// A completion carrying a secret is redacted on the way back out too — the
// caller persists what it gets, so the wrapper cannot be the inner layer.
func TestDecorateDaemonLLMClient_RedactsTheCompletion(t *testing.T) {
	ac := &config.Config{}
	ac.SecretsSanitization.Enabled = true

	secret := "sk-ant-" + "api03-AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555FFFF6666"
	spy := &spySummarizeClient{
		completeFunc: func(system, userMsg string, opts ...extraction.CallOption) (string, error) {
			return "leaked " + secret + " leaked", nil
		},
	}

	out, err := decorateDaemonLLMClient(spy, ac, llmClientOpts{}).Complete("sys", "user")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("the completion left the client unredacted: %q", out)
	}
}

// secrets_sanitization.enabled=false is the live configuration: the client must
// come out exactly as it went in, budget and gate only.
func TestDecorateDaemonLLMClient_LeavesTheClientAloneWhenDisabled(t *testing.T) {
	ac := &config.Config{}
	ac.SecretsSanitization.Enabled = false

	secret := "AKIAIOSFODNN7EXAMPLE"
	var seen string
	spy := &spySummarizeClient{
		completeFunc: func(system, userMsg string, opts ...extraction.CallOption) (string, error) {
			seen = userMsg
			return "ok", nil
		},
	}

	client := decorateDaemonLLMClient(spy, ac, llmClientOpts{})
	if _, err := client.Complete("sys", secret); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if seen != secret {
		t.Errorf("without a redactor the prompt must pass through untouched, got %q", seen)
	}
}

// Without a tracker the handle stays unlimited, exactly as before.
func TestDecorateDaemonLLMClient_NoTrackerIsUnlimited(t *testing.T) {
	ac := &config.Config{}
	spy := &spySummarizeClient{}
	client := decorateDaemonLLMClient(spy, ac, llmClientOpts{})
	if !extraction.HasBudget(client) {
		t.Error("a client without a configured budget must stay available")
	}
}

// The per-call cap is a CLI flag, and it is only visible end to end: the fake
// binary records the argv the wrapper handed it.
func TestDecorateDaemonLLMClient_AppliesThePerCallCapOnCLIHandles(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	bin := filepath.Join(dir, "fake-claude")
	script := "#!/bin/sh\nprintf '%s ' \"$@\" > " + argsFile + "\nprintf '%s' '{\"result\":\"ok\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	ac := &config.Config{}
	raw, err := extraction.NewLLMClient("cli", "", "test-model", bin, "")
	if err != nil {
		t.Fatalf("raw cli client: %v", err)
	}

	client := decorateDaemonLLMClient(raw, ac, llmClientOpts{maxPerCall: 2.50})
	if _, err := client.Complete("sys", "user"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read captured argv: %v", err)
	}
	if !strings.Contains(string(args), "--max-budget-usd 2.50") {
		t.Fatalf("the per-call cap did not reach the CLI client; argv was: %s", args)
	}
}

// And without a configured cap the flag must not appear.
func TestDecorateDaemonLLMClient_NoCapMeansNoFlag(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	bin := filepath.Join(dir, "fake-claude")
	script := "#!/bin/sh\nprintf '%s ' \"$@\" > " + argsFile + "\nprintf '%s' '{\"result\":\"ok\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	ac := &config.Config{}
	raw, err := extraction.NewLLMClient("cli", "", "test-model", bin, "")
	if err != nil {
		t.Fatalf("raw cli client: %v", err)
	}

	if _, err := decorateDaemonLLMClient(raw, ac, llmClientOpts{}).Complete("sys", "user"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	args, _ := os.ReadFile(argsFile)
	if strings.Contains(string(args), "--max-budget-usd") {
		t.Fatalf("no cap configured, but the flag appeared: %s", args)
	}
}

// A metered client is wrapped. The auto-correct path used to opt out of the
// wrapper (its prompt carried the cap body); that opt-out is gone, and this pins
// the shape every metered site now shares.
func TestDecorateDaemonLLMClient_WrapsAMeteredClient(t *testing.T) {
	ac := &config.Config{}
	ac.SecretsSanitization.Enabled = true
	raw := &autoCorrectStubClient{}

	if _, ok := decorateDaemonLLMClient(raw, ac, meteredOpts(llmClientEnv{}, "", "extract")).(*extraction.SanitizingClient); !ok {
		t.Error("a metered client must be wrapped: its prompt is redacted like every other prompt")
	}

	ac.SecretsSanitization.Enabled = false
	if _, ok := decorateDaemonLLMClient(raw, ac, llmClientOpts{}).(*extraction.SanitizingClient); ok {
		t.Error("with sanitization disabled nothing may be wrapped")
	}
}
