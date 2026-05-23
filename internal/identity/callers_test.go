package identity_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEnsureCallerSet pins the production call graph of identity.Ensure to
// the two sanctioned edges: openRuntime (every command that opens the
// store) and cmdWhoami (whoami answers from anywhere, including outside a
// git repo, so it can't route through openRuntime). A third edge is the
// regression class this guardrail exists to catch — any future command
// resolving identity directly bypasses the one-edge-per-process discipline
// and reintroduces the env-drift bug Ensure's contract is meant to prevent.
//
// Do not silence this test by adding a sync.Once around Ensure. Memoizing
// hides the very env-flipping that identity tests document as legal; the
// discipline belongs in the call graph, not behind a cache.
func TestEnsureCallerSet(t *testing.T) {
	want := map[string]struct{}{
		"openRuntime": {},
		"cmdWhoami":   {},
	}

	got := ensureCallers(t, "../cli")

	for fn := range got {
		if _, ok := want[fn]; !ok {
			t.Errorf("identity.Ensure has a new production caller %q in internal/cli; "+
				"if it's a legitimate resolution edge, add it to the guardrail set and "+
				"document why it can't read rt.Agent. Do not add a sync.Once to Ensure.", fn)
		}
	}
	for fn := range want {
		if _, ok := got[fn]; !ok {
			t.Errorf("expected identity.Ensure caller %q not found; refactor moved it?", fn)
		}
	}
}

// ensureCallers returns the set of enclosing function names in dir whose
// bodies contain a call to identity.Ensure. Test files are excluded;
// aliased imports of loto/internal/identity are resolved per file.
func ensureCallers(t *testing.T, dir string) map[string]struct{} {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]struct{}{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		alias := identityImportName(file)
		if alias == "" {
			continue
		}
		collectEnsureCallers(file, alias, out)
	}
	return out
}

// identityImportName returns the local name that loto/internal/identity is
// bound to in file (e.g. "identity" by default, or "id" for an aliased
// import), or "" if the package is not imported.
func identityImportName(file *ast.File) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if path != "loto/internal/identity" {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "identity"
	}
	return ""
}

// collectEnsureCallers walks file and, for every CallExpr matching
// <alias>.Ensure(), records the enclosing top-level function name in out.
func collectEnsureCallers(file *ast.File, alias string, out map[string]struct{}) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if ident.Name == alias && sel.Sel.Name == "Ensure" {
				out[fn.Name.Name] = struct{}{}
			}
			return true
		})
	}
}
