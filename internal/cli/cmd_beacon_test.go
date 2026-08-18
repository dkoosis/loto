package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCmdBeacon_ToleratesMissingPath is loto-z5nb's CLI-level AC: a beacon
// for a path that has not been created yet must succeed, not "not-found" —
// this is the case a Write tool call CREATING a new file needs protected,
// and it was the one gap left when a Write beacon reached the CLI at all.
func TestCmdBeacon_ToleratesMissingPath(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	newFile := "brand-new.go"

	var out, errBuf bytes.Buffer
	code := Run([]string{"beacon", newFile}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("beacon on a missing path: exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ beacon count=1") {
		t.Errorf("missing success row: %q", out.String())
	}

	// The path still does not exist on disk — a beacon announces, it never
	// creates anything.
	if _, statErr := os.Lstat(filepath.Join(repo, newFile)); statErr == nil {
		t.Error("beacon must not create the file it announces")
	}
}

// TestCmdBeacon_StillRefusesSymlink pins the carve-out's boundary at the CLI
// layer too: a beacon whose path IS a symlink must still be refused, exactly
// like a plain lock — the relaxation is ENOENT-only.
func TestCmdBeacon_StillRefusesSymlink(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	realFile := filepath.Join(repo, tcTargetA) // withTempProject already seeds this file
	sym := filepath.Join(repo, "sym.go")
	if err := os.Symlink(realFile, sym); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{"beacon", "sym.go"}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("beacon on a symlink must be refused, got exit 0: out=%q", out.String())
	}
	if !strings.Contains(errBuf.String(), "symlink") {
		t.Errorf("refusal must name the reason: %q", errBuf.String())
	}
}
