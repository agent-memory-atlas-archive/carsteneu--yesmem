package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// redactorExemptions lists the functions whose clients are wrapped further down
// the call chain instead of at the construction site. The reason has to state
// where the wrap happens, otherwise the entry is just a hole in the guard.
var redactorExemptions = map[string]string{
	"cmd_maintenance.go:runQuickstart": "client und qualityClient werden in daemon.RunQuickstart gewrappt (internal/daemon/quickstart.go:20-24)",
}

// TestPackageMainLLMClientsAreRedacted haelt die Redactor-Deckung in package
// main. Die Fabrik in internal/daemon/llm_client_factory.go deckt nur die
// Daemon-Pfade ab; jedes CLI-Kommando baut seinen Client selbst und muss ihn
// ueber redacted() fuehren, sonst geht Session-Text unredigiert an den Provider.
//
// Der Test ist ein Struktur-Scan, kein Verhaltensbeweis: er verlangt, dass jeder
// aus extraction.NewLLMClient gebaute Client innerhalb derselben Funktion an
// redacted() uebergeben wird.
func TestPackageMainLLMClientsAreRedacted(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			if _, exempt := redactorExemptions[path+":"+fn.Name.Name]; exempt {
				return true
			}
			built, wrapped := collectClientWiring(fn.Body)
			names := make([]string, 0, len(built))
			for name := range built {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				checked++
				if !wrapped[name] {
					t.Errorf("%s: LLM-Client %q wird ohne redacted() an den Provider gereicht",
						fset.Position(built[name]), name)
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("kein extraction.NewLLMClient-Aufruf gefunden; der Scan greift nicht mehr")
	}
}

// collectClientWiring liefert die Namen, die in body aus extraction.NewLLMClient
// gebaut werden, und die Namen, die in body wirksam geredet werden.
//
// Gewrappt zaehlt nur, wenn der Wrapper das Ergebnis auch bindet: bei
// "client = redacted(cfg, client)" muss die linke Seite denselben Namen tragen,
// und "_ = redacted(cfg, client)" ist ein Verstoss, weil der rohe Client danach
// weiterbenutzt wird. Ein redacted()-Aufruf als Argument einer anderen Funktion
// (NewTwoPassExtractor(client, redacted(cfg, extractClient), store)) zaehlt,
// weil der Client dort nur noch durch den Wrapper erreichbar ist.
func collectClientWiring(body *ast.BlockStmt) (built map[string]token.Pos, wrapped map[string]bool) {
	built = map[string]token.Pos{}
	wrapped = map[string]bool{}
	boundInAssignment := map[token.Pos]bool{}
	recordBuilt := func(lhs ast.Expr, values []ast.Expr) {
		if len(values) != 1 {
			return
		}
		call, ok := values[0].(*ast.CallExpr)
		if !ok || !isBuilderCall(call) {
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
			if !ok || !isLocalCall(call, "redacted") {
				break
			}
			boundInAssignment[call.Pos()] = true
			lhs, ok := s.Lhs[0].(*ast.Ident)
			if ok && lhs.Name != "_" && wrapsIdent(call, lhs.Name) {
				wrapped[lhs.Name] = true
			}
		case *ast.ValueSpec:
			if len(s.Names) > 0 {
				recordBuilt(s.Names[0], s.Values)
			}
		case *ast.CallExpr:
			if isLocalCall(s, "redacted") && !boundInAssignment[s.Pos()] {
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

func wrapsIdent(call *ast.CallExpr, name string) bool {
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == name {
			return true
		}
	}
	return false
}

func isBuilderCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewLLMClient" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "extraction"
}

// TestPackageMainRedactorGuardRejectsDiscardedWrapper haelt den Guard selbst
// scharf. Ein "_ = redacted(cfg, client)" erzeugt einen Wrapper und verwirft ihn
// wieder; der rohe Client bleibt in Gebrauch. Ohne diesen Test waere die
// Bindung an die linke Seite der Zuweisung nicht abgesichert.
func TestPackageMainRedactorGuardRejectsDiscardedWrapper(t *testing.T) {
	const src = `package main

func bad() {
	client, err := extraction.NewLLMClient()
	if err != nil {
		return
	}
	_ = redacted(nil, client)
	use(client)
}

func good() {
	client, err := extraction.NewLLMClient()
	if err != nil {
		return
	}
	client = redacted(nil, client)
	use(client)
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "probe.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range f.Decls {
		decl, ok := fn.(*ast.FuncDecl)
		if !ok {
			continue
		}
		built, wrapped := collectClientWiring(decl.Body)
		if _, ok := built["client"]; !ok {
			t.Fatalf("%s: client wurde nicht als gebaut erkannt", decl.Name.Name)
		}
		switch decl.Name.Name {
		case "bad":
			if wrapped["client"] {
				t.Fatal("verworfener Wrapper zaehlt als geredet; der Guard haelt den Bypass nicht")
			}
		case "good":
			if !wrapped["client"] {
				t.Fatal("gebundener Wrapper zaehlt nicht als geredet")
			}
		}
	}
}

func isLocalCall(call *ast.CallExpr, name string) bool {
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == name
}
