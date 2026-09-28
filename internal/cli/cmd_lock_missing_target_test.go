package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// loto-i382: `loto lock` locking a not-yet-existing file inside the repo,
// instead of refusing reason=not-found, works by default (no --create flag —
// dk's call on the bead). This file covers the rest of the bead's AC beyond
// TestLock_AllowsNonExistentTargetInRepo (cmd_lock_test.go): a peer is still
// blocked, repo-escape/not-regular-file refusals are unchanged, creating the
// file afterward keeps the same lock, and — dk's added AC — a case-insensitive
// filesystem folds the missing-path spelling to the same key the later,
// differently-cased, real file resolves to.

// TestLock_MissingParentDirectoryStillRefused pins the bead's Rule text
// exactly: "A path inside the repo that does not exist yet, and whose parent
// directory exists, can be locked." A missing target whose parent ALSO does
// not exist is still refused reason=not-found — the same shape a bare
// missing file always reported — because `loto lock` alone (not
// beacon/hookAdmit) enforces the parent-exists precondition.
func TestLock_MissingParentDirectoryStillRefused(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "nodir/file.go", "-t", tcIntentTest}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "not-found") {
		t.Errorf("expected reason=not-found: %q", combined)
	}
}

// TestLock_MissingFileLockBlocksPeer is AC2: a peer's `loto lock` on the same
// not-yet-existing path is blocked exactly like it would be for a real file.
func TestLock_MissingFileLockBlocksPeer(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	if err := os.Mkdir(filepath.Join(repo, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	const target = "new/file.go"

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid())) // durable live holder → hard block
	if code := Run([]string{tcCmdLock, target, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock on missing %q failed", target)
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, target, "-t", tcIntentWrite}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "✗ blocked") || !strings.Contains(combined, "blocker=") {
		t.Errorf("expected blocker report: %q", combined)
	}
}

// TestLock_MissingFileOutsideRepoStillRepoEscape is AC3's first half: a
// not-yet-existing path outside the repo must still be refused repo-escape,
// not admitted as "missing but lockable". The not-regular-file half of AC3
// (`loto lock somedir`) is unaffected by allowMissing and stays covered by
// TestLock_RejectDirectoryTarget.
func TestLock_MissingFileOutsideRepoStillRepoEscape(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "../outside.go", "-t", tcIntentTest}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "repo-escape") {
		t.Errorf("expected reason=repo-escape: %q", combined)
	}
}

// TestLock_CreatingFileAfterLockKeepsSameLockRow is AC4: creating the file
// after locking it while missing must not mint a second row — the canonical
// key is computed from the resolved parent + name and does not depend on
// whether anything is on disk yet (foldTargetKey already works this way; this
// pins `loto lock`'s own acquire path to the same fact).
//
// ‡ cubic review on #388: a store-level row count alone does not prove the
// NOW-EXISTING file resolves to that same key — only that nothing minted a
// second row under some other key. A second agent's `loto lock` on the
// now-real file, observed through the CLI, is the behavior the "same key"
// claim actually cashes out as: if the file resolved to any key other than
// the one alice holds, bob's lock would silently succeed.
func TestLock_CreatingFileAfterLockKeepsSameLockRow(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	if err := os.Mkdir(filepath.Join(repo, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	const target = "new/file.go"

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid())) // durable live holder → hard block
	if code := Run([]string{tcCmdLock, target, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock on missing %q failed", target)
	}

	if err := os.WriteFile(filepath.Join(repo, "new", "file.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	rt, err := openRuntime(context.Background())
	if err != nil {
		t.Fatalf("open runtime: %v", err)
	}
	held, err := rt.Store.ListLocks(rt.Ctx)
	rt.Close()
	if err != nil {
		t.Fatalf("list locks: %v", err)
	}
	if len(held) != 1 {
		t.Fatalf("want exactly one row after creating the locked file, got %d: %+v", len(held), held)
	}
	if held[0].Target.Canonical != target {
		t.Errorf("canonical key = %q, want %q unchanged by the file's later creation", held[0].Target.Canonical, target)
	}

	// The observable proof: bob locking the now-real file must still hit
	// alice's row, not a fresh, unheld one.
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, target, "-t", tcIntentWrite}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("expected bob blocked on the now-real file, got exit %d; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "✗ blocked") || !strings.Contains(combined, "blocker=") {
		t.Errorf("expected blocker report: %q", combined)
	}
}

// TestLockCaseVariantThenCreateSharesOneKey is dk's added AC (bead comment,
// 2026-09-28): on a case-insensitive filesystem, lock New.go while it does
// not exist, then create new.go — one row, same key, because the key is
// built from the resolved parent + name (foldTargetKey), not from a disk
// walk that only exists to find on a real file.
//
// ‡ Filesystem-conditional like the other case_variant_paths_test.go cases:
// only the machine's own FS can exercise its branch. Where the FS is
// case-sensitive, New.go and new.go are two different files, and the
// assertion is the mirror one — the original lock on New.go is untouched.
//
// ‡ cubic review on #388: a bare row count does not prove the now-real
// LOWER spelling actually resolves to alice's key — a second agent's
// `loto lock` on the now-real spelling, observed through the CLI, does: it
// must be blocked where the FS folds, and must succeed (a genuinely
// different, unheld file) where it does not.
func TestLockCaseVariantThenCreateSharesOneKey(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	folds := caseVariantFSNote(t, repo)
	const upper, lower = "New.go", "new.go"

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid())) // durable live holder → hard block
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdLock, upper, "-t", tcIntentTest}, &out, &errBuf); code != 0 {
		t.Fatalf("alice lock on missing %q: exit=%d out=%q err=%q", upper, code, out.String(), errBuf.String())
	}

	if err := os.WriteFile(filepath.Join(repo, lower), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	rt, err := openRuntime(context.Background())
	if err != nil {
		t.Fatalf("open runtime: %v", err)
	}
	held, err := rt.Store.ListLocks(rt.Ctx)
	rt.Close()
	if err != nil {
		t.Fatalf("list locks: %v", err)
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var bobOut, bobErr bytes.Buffer
	bobCode := Run([]string{tcCmdLock, lower, "-t", tcIntentWrite}, &bobOut, &bobErr)
	bobCombined := bobOut.String() + bobErr.String()

	if folds {
		if len(held) != 1 {
			t.Fatalf("case-insensitive FS: want one row after creating %q under the spelling locked as %q, got %d: %+v", lower, upper, len(held), held)
		}
		if held[0].Target.Canonical != lower {
			t.Errorf("canonical key = %q, want the folded spelling %q (resolved parent + name)", held[0].Target.Canonical, lower)
		}
		if bobCode != 1 || !strings.Contains(bobCombined, "✗ blocked") {
			t.Fatalf("case-insensitive FS: bob's lock on the now-real %q must hit alice's row; exit=%d %q", lower, bobCode, bobCombined)
		}
		return
	}
	// Case-sensitive filesystem: New.go and new.go are genuinely different
	// files, so creating new.go must not touch the lock held on New.go, and
	// bob must be able to lock the untouched, unheld new.go outright.
	if len(held) != 1 || held[0].Target.Canonical != upper {
		t.Fatalf("case-sensitive FS: want the original lock on %q untouched, got %+v", upper, held)
	}
	if bobCode != 0 {
		t.Fatalf("case-sensitive FS: bob's lock on the untouched, unheld %q should succeed; exit=%d %q", lower, bobCode, bobCombined)
	}
}
