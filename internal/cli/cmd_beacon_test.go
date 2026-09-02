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
//
// It is also the loto-3tv3 acceptance case for lock. withTempProject seeds
// repo/a.go, so the bare token `a.go` typed from repo/sub is the
// same-basename-at-root collision: before the fix it minted the key `a.go` —
// exit 0, wrong file, silently. The key assertion below is what makes that
// visible; exit 0 alone passed the whole time the bug was live.
func TestCmdLock_ExistingFileFromNestedCwd(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, tcTargetA), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", "nested cwd"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("lock from a nested cwd: exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}

	var st bytes.Buffer
	if code := Run([]string{tcCmdStatus, tcFlagMine}, &st, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status: exit=%d %q", code, st.String())
	}
	if !strings.Contains(st.String(), "target=sub/a.go") {
		t.Errorf("lock must key the file the caller sees: %q", st.String())
	}
	if strings.Contains(st.String(), "target=a.go") {
		t.Errorf("lock keyed the repo-root file instead: %q", st.String())
	}
}

// TestCmdUnlock_FromSubdirReleasesTheFileTheCallerMeans pins the lock/unlock
// key round-trip. If the two verbs minted different keys for one token, unlock
// could not find its own lock and the row would sit there until TTL.
func TestCmdUnlock_FromSubdirReleasesTheFileTheCallerMeans(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, tcTargetA), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	if code := Run([]string{tcCmdLock, tcTargetA, "-t", "round trip"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock from a nested cwd failed")
	}
	if code := Run([]string{tcCmdUnlock, tcTargetA, "-t", tcIntentDone}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("unlock from a nested cwd failed")
	}
	var st bytes.Buffer
	if code := Run([]string{tcCmdStatus, tcFlagMine}, &st, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status: exit=%d %q", code, st.String())
	}
	if strings.Contains(st.String(), "target=") {
		t.Errorf("unlock must release the row lock minted: %q", st.String())
	}
}

// TestCmdBeacon_RefusesShellToken is loto-bl66's CLI-level AC, and it pins
// BOTH halves in one test because they pull against each other: beacon must
// refuse a token that cannot be a path, while still accepting a well-formed
// path that does not exist yet — the second is beacon's whole reason to exist,
// so a fix that bought the first by giving up the second is not a fix.
//
// The live defect was a lock on the literal string "$FAKE_HOME", minted by the
// PreToolUse gate. Nothing releases such a row but TTL: no scan can reconcile
// a canonical that is not a path.
func TestCmdBeacon_RefusesShellToken(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	for _, tok := range []string{"$FAKE_HOME", "$PROBE_VAR", "`whoami`", " leading.go"} {
		var out, errBuf bytes.Buffer
		code := Run([]string{gateIntentBeacon, tok}, &out, &errBuf)
		if code == 0 {
			t.Errorf("beacon %q: exit=0, want a refusal", tok)
		}
		combined := out.String() + errBuf.String()
		if !strings.Contains(combined, "not-a-path") {
			t.Errorf("beacon %q: want reason=not-a-path, got %q", tok, combined)
		}
		// AC: the refusal is visible, never a silent skip.
		if !strings.Contains(combined, "✗") {
			t.Errorf("beacon %q: refusal must render a ✗ row, got %q", tok, combined)
		}
	}

	// The other half, restated here so a regression on either shows up in one
	// failing test rather than two unrelated ones.
	var out, errBuf bytes.Buffer
	if code := Run([]string{gateIntentBeacon, "still-uncreated.go"}, &out, &errBuf); code != 0 {
		t.Fatalf("beacon on a well-formed missing path: exit=%d out=%q err=%q",
			code, out.String(), errBuf.String())
	}
}
