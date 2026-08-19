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
	code := Run([]string{gateIntentBeacon, newFile}, &out, &errBuf)
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
	code := Run([]string{gateIntentBeacon, "sym.go"}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("beacon on a symlink must be refused, got exit 0: out=%q", out.String())
	}
	if !strings.Contains(errBuf.String(), "symlink") {
		t.Errorf("refusal must name the reason: %q", errBuf.String())
	}
}

// TestCmdBeacon_SymlinkRefusedFromNestedCwd is the Codex #258 P1 regression:
// validation stats the REPO-relative canonical, so it must resolve under
// repoTop, not the process CWD. From a subdirectory the bare stat resolved
// "sub/sym.go" as "<repo>/sub/sub/sym.go" -> ENOENT -> the allowMissing
// carve-out silently ADMITTED the beacon, skipping the symlink check the path
// would have failed. The refusal must be identical from any cwd in the repo.
func TestCmdBeacon_SymlinkRefusedFromNestedCwd(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, tcTargetA), filepath.Join(sub, "sym.go")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	var out, errBuf bytes.Buffer
	code := Run([]string{gateIntentBeacon, "sym.go"}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("beacon on a symlink must be refused from a nested cwd too, got exit 0: out=%q", out.String())
	}
	if !strings.Contains(errBuf.String(), "symlink") {
		t.Errorf("refusal must name the reason: %q", errBuf.String())
	}
}

// TestCmdLock_ExistingFileFromNestedCwd is the other half of the same Codex
// #258 P1: with the stat resolved against CWD, an ordinary `loto lock` on a
// real file reported a spurious "not-found" whenever loto ran anywhere but
// the repo root.
func TestCmdLock_ExistingFileFromNestedCwd(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	var out, errBuf bytes.Buffer
	code := Run([]string{"lock", "a.go", "-t", "nested cwd"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("lock from a nested cwd: exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
}
