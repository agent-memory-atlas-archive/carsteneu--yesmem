package setup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/extraction"
)

// TestSetupLLMClientsAreRedacted haelt die Redactor-Deckung in internal/setup.
// Setup baut seine Clients ausserhalb der Daemon-Fabrik; ohne redactedForDataDir
// geht der Prompt unredigiert an den Provider, auch wenn
// secrets_sanitization.enabled gesetzt ist.
//
// Struktur-Scan, kein Verhaltensbeweis: jeder aus extraction.NewLLMClient
// gebaute Client muss in derselben Funktion an redactedForDataDir() gehen.
// TestAPIKeyForSonnet baut seinen Client ueber extraction.NewClient und sendet
// nur ein festes "Reply with OK."; er faellt bewusst nicht unter diesen Scan.
func TestSetupLLMClientsAreRedacted(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			built, wrapped := los06cCollectClientWiring(fn.Body)
			names := make([]string, 0, len(built))
			for n := range built {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				checked++
				if !wrapped[n] {
					t.Errorf("%s: LLM-Client %q wird ohne redactedForDataDir() an den Provider gereicht",
						fset.Position(built[n]), n)
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("kein extraction.NewLLMClient-Aufruf gefunden; der Scan greift nicht mehr")
	}
}

// los06cCollectClientWiring liefert die Namen, die in body aus
// extraction.NewLLMClient gebaut werden, und die Namen, die in body wirksam
// geredet werden. Gewrappt zaehlt nur, wenn der Wrapper das Ergebnis auch
// bindet: "client = redactedForDataDir(dataDir, client)" muss links denselben
// Namen tragen, "_ = redactedForDataDir(...)" ist ein Verstoss. Ein Aufruf als
// Argument einer anderen Funktion zaehlt, weil der Client dort nur noch durch
// den Wrapper erreichbar ist.
func los06cCollectClientWiring(body *ast.BlockStmt) (built map[string]token.Pos, wrapped map[string]bool) {
	built = map[string]token.Pos{}
	wrapped = map[string]bool{}
	boundInAssignment := map[token.Pos]bool{}
	recordBuilt := func(lhs ast.Expr, values []ast.Expr) {
		if len(values) != 1 {
			return
		}
		call, ok := values[0].(*ast.CallExpr)
		if !ok || !los06cIsBuilderCall(call) {
			return
		}
		if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
			built[id.Name] = call.Pos()
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) == 0 || len(s.Rhs) != 1 {
				break
			}
			recordBuilt(s.Lhs[0], s.Rhs)
			call, ok := s.Rhs[0].(*ast.CallExpr)
			if !ok || !los06cIsLocalCall(call, "redactedForDataDir") {
				break
			}
			boundInAssignment[call.Pos()] = true
			lhs, ok := s.Lhs[0].(*ast.Ident)
			if ok && lhs.Name != "_" && los06cWrapsIdent(call, lhs.Name) {
				wrapped[lhs.Name] = true
			}
		case *ast.ValueSpec:
			if len(s.Names) > 0 {
				recordBuilt(s.Names[0], s.Values)
			}
		case *ast.CallExpr:
			if los06cIsLocalCall(s, "redactedForDataDir") && !boundInAssignment[s.Pos()] {
				for _, a := range s.Args {
					if id, ok := a.(*ast.Ident); ok {
						wrapped[id.Name] = true
					}
				}
			}
		}
		return true
	})
	return built, wrapped
}

func los06cWrapsIdent(call *ast.CallExpr, name string) bool {
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == name {
			return true
		}
	}
	return false
}

func los06cIsBuilderCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewLLMClient" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "extraction"
}

func los06cIsLocalCall(call *ast.CallExpr, name string) bool {
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == name
}

type los06cStubLLMClient struct {
	system string
	user   string
}

func (s *los06cStubLLMClient) Complete(system, userMsg string, _ ...extraction.CallOption) (string, error) {
	s.system, s.user = system, userMsg
	return "ok", nil
}

func (s *los06cStubLLMClient) CompleteJSON(system, userMsg string, _ map[string]any, _ ...extraction.CallOption) (string, error) {
	s.system, s.user = system, userMsg
	return "{}", nil
}

func (s *los06cStubLLMClient) Name() string  { return "los06c-stub" }
func (s *los06cStubLLMClient) Model() string { return "stub" }

func TestLOS06CRedactedForDataDirRedactsPrompt(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("secrets_sanitization:\n  enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	inner := &los06cStubLLMClient{}
	wrapped := redactedForDataDir(dataDir, inner)
	if _, ok := wrapped.(*extraction.SanitizingClient); !ok {
		t.Fatalf("client was not wrapped despite secrets_sanitization.enabled: %T", wrapped)
	}

	secret := "sk-ant-" + "api03-" + strings.Repeat("a", 60)
	if _, err := wrapped.Complete("sys", "use "+secret+" now"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(inner.user, secret) {
		t.Fatalf("secret reached the provider unwrapped: %q", inner.user)
	}
	if !strings.Contains(inner.user, "[REDACTED:anthropic_api_key]") {
		t.Fatalf("expected the redaction marker in the forwarded prompt: %q", inner.user)
	}
}

func TestLOS06CRedactedForDataDirPassThrough(t *testing.T) {
	inner := &los06cStubLLMClient{}
	if got := redactedForDataDir(t.TempDir(), inner); got != extraction.LLMClient(inner) {
		t.Fatalf("missing config must leave the client untouched, got %T", got)
	}

	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("secrets_sanitization:\n  enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := redactedForDataDir(dataDir, inner); got != extraction.LLMClient(inner) {
		t.Fatalf("disabled switch must leave the client untouched, got %T", got)
	}
	if got := redactedForDataDir(dataDir, nil); got != nil {
		t.Fatalf("nil client must stay nil, got %T", got)
	}
}
