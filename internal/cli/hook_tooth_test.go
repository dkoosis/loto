package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tcBannedInputRead is the dotted field name §10's first tooth forbids: the
// harness event's tool input, dot, the command string. Spelled by
// concatenation so the check's own source is not a hit for itself.
var tcBannedInputRead = "tool_input" + "." + "command"

// TestNoHookReadsTheCommandString is enforcement-design.md §10's first tooth,
// as a test rather than a review habit.
//
// The design deletes ~2,000 lines that tried to prevent a working-tree
// mutation by parsing the Bash command string; that parser produced 10 false
// refusals and prevented 1 incident, because the set of commands that can
// write a file is unbounded and no parser over it is complete. A ratified
// constraint needs a check that goes red, and the check tests the constraint
// itself — "no hook reads the command string" — not a proxy for it such as a
// line budget (§10 item 2, struck 2026-09-09).
//
// Leg 1 is the grep the bead's acceptance criterion names. Leg 2 is the reason
// the grep stays true: the decode struct declares one field, and encoding/json
// discards every key a struct does not declare, so the value never exists in
// this process at all.
func TestNoHookReadsTheCommandString(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/cli: %v", err)
	}
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || name == "hook_tooth_test.go" {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		if strings.Contains(string(b), tcBannedInputRead) {
			t.Errorf("%s reads %s — enforcement-design.md §10 tooth 1: no hook decides over the command string", name, tcBannedInputRead)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no source files; the tooth graded nothing")
	}
}

// TestHookToolInputDeclaresOnlyFilePath is leg 2: the struct the harness event
// decodes into carries exactly one field, and it is the structured file path.
// A field added here is how the deleted parser would come back, so the shape
// is asserted rather than trusted.
func TestHookToolInputDeclaresOnlyFilePath(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "cmd_hook.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cmd_hook.go: %v", err)
	}
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "hookToolInput" {
			return true
		}
		found = true
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			t.Fatal("hookToolInput is not a struct")
		}
		if got := len(st.Fields.List); got != 1 {
			t.Fatalf("hookToolInput declares %d fields; exactly one (FilePath) is permitted", got)
		}
		if name := st.Fields.List[0].Names[0].Name; name != "FilePath" {
			t.Errorf("hookToolInput's only field is %q, want FilePath", name)
		}
		return false
	})
	if !found {
		t.Fatal("hookToolInput not found; the tooth graded nothing")
	}
}
