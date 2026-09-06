package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Case-variant path regression tests (loto-f8m8, loto-8soe).
//
// The bug: every conflict decision — lock overlap, claim prefix overlap,
// `check --gate` admission — is a byte comparison over Target.Canonical. On a
// case-insensitive filesystem `internal/store/x.go` and `internal/Store/x.go`
// are ONE file and were TWO keys, so two agents were granted the same file.
// cdc2eeb dropped fs-case detection on the premise that "case-insensitive
// filesystems get the OS resolution" — true when opening a file, false for a
// string comparison that never touches the filesystem.
//
// ‡ These tests are filesystem-conditional by necessity: only the machine's
// own FS can exercise its branch. The linux CI leg proves the two spellings
// stay independent where they are genuinely two files; the darwin leg (weekly
// backstop, or a `ci:macos` label) proves they converge where they are one.
// Both branches are asserted in every test rather than skipped, so neither
// runner is silently proving nothing.

// tcStoreVariantGo is tcStoreStoreGo with the directory segment spelled in the
// other case — the two tokens whose canonical keys must converge on a
// case-insensitive filesystem and stay apart on a case-sensitive one.
const tcStoreVariantGo = "internal/Store/store.go"

// caseVariantSkipUnless reports the FS class once per test, so a failure names
// which branch ran rather than leaving the reader to guess from the runner OS.
func caseVariantFSNote(t *testing.T, repo string) bool {
	t.Helper()
	ci := caseInsensitiveFS(repo)
	t.Logf("filesystem at %s is case-insensitive=%v", repo, ci)
	return ci
}

// TestProbeFoldedPathRejectsSymlinkAlias: a case-variant symlink beside the
// real entry makes two spellings stat to one inode without the filesystem
// folding anything. Reading that as "case-insensitive" would let foldTargetKey
// mint one key for A.go and a.go, so a lock on one would refuse a peer
// standing on the other (codex review on #306).
//
// Only a case-sensitive filesystem can hold the two entries, so the case-folding
// runner skips — which is correct: there the alias cannot be created at all.
func TestProbeFoldedPathRejectsSymlinkAlias(t *testing.T) {
	parent := t.TempDir()
	realDir := filepath.Join(parent, "repo")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "Repo")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("cannot create a case-variant alias here (case-folding filesystem): %v", err)
	}

	got, ok := probeFoldedPath(realDir)
	if !ok {
		t.Fatal("probeFoldedPath declined to answer for an existing directory")
	}
	if got {
		t.Error("probeFoldedPath read a symlink alias as filesystem case folding")
	}
	if caseInsensitiveFS(realDir) {
		t.Error("caseInsensitiveFS reported folding on a case-sensitive filesystem")
	}
}

// TestCaseInsensitiveFSEmptyRepo: a freshly `git init`'d, otherwise empty repo
// top has zero entries and (on a temp checkout) a digit-only basename — the
// exact shape that made the pre-fix probe fall back to its "case-sensitive"
// default by the absence of a flippable name, rather than answer from the
// filesystem (loto-l59r). `top` below reproduces that shape directly, since
// caseInsensitiveFS never looks past dir's own contents.
//
// witness independently establishes this run's actual filesystem class from a
// sibling directory that already has an entry to flip, so the assertion holds
// on both a case-folding and a case-sensitive runner rather than assuming one.
func TestCaseInsensitiveFSEmptyRepo(t *testing.T) {
	parent := t.TempDir()

	witness := filepath.Join(parent, "witness")
	if err := os.Mkdir(witness, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(witness, "probe"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wantFolds := caseInsensitiveFS(witness)
	t.Logf("filesystem at %s is case-insensitive=%v", parent, wantFolds)

	top := filepath.Join(parent, "1234")
	if err := os.Mkdir(top, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := caseInsensitiveFS(top); got != wantFolds {
		t.Errorf("caseInsensitiveFS(empty repo top) = %v; want %v (this filesystem's actual fold behavior) — answered by name-shape, not by disk (loto-l59r)", got, wantFolds)
	}
}

// TestFoldTargetKeyFoldsSpelling: the key a variant spelling produces must be
// the folded one where the FS folds, and the caller's spelling where it does
// not.
func TestFoldTargetKeyFoldsSpelling(t *testing.T) {
	repo := withTempProject(t) // creates internal/store/store.go

	got := foldTargetKey(nil, repo, tcStoreVariantGo)
	if caseVariantFSNote(t, repo) {
		if got != tcStoreStoreGo {
			t.Errorf("foldTargetKey(%q) = %q; want %q — every spelling of one file is one key", tcStoreVariantGo, got, tcStoreStoreGo)
		}
		return
	}
	if got != tcStoreVariantGo {
		t.Errorf("foldTargetKey(%q) = %q; want it unchanged on a case-sensitive filesystem", tcStoreVariantGo, got)
	}
}

// TestFoldTargetKeyFoldsAPathThatDoesNotExist is the first of loto-8soe's two
// doors, at unit scale: `loto beacon` names a file nobody has written yet, and
// the old disk walk stopped at the first absent segment and kept the caller's
// casing — so two spellings of one future file were two keys. The key must not
// depend on whether anything is on disk.
func TestFoldTargetKeyFoldsAPathThatDoesNotExist(t *testing.T) {
	repo := withTempProject(t)
	folds := caseVariantFSNote(t, repo)
	// One tail segment absent, then every segment absent — the second is what a
	// claim on a directory nobody has scaffolded yet resolves to.
	cases := []struct{ typed, folded string }{
		{"internal/Store/NotYetWritten.go", "internal/store/notyetwritten.go"},
		{"Nowhere/AtAll.go", "nowhere/atall.go"},
	}
	for _, c := range cases {
		want := c.typed
		if folds {
			want = c.folded
		}
		if got := foldTargetKey(nil, repo, c.typed); got != want {
			t.Errorf("foldTargetKey(%q) = %q; want %q — the key must not depend on what is on disk", c.typed, got, want)
		}
	}
}

// TestFoldTargetKeyLeavesUnfoldableShapes covers the early returns: an empty
// repo frame, "", ".", and an absolute path. None is a repo-relative key.
func TestFoldTargetKeyLeavesUnfoldableShapes(t *testing.T) {
	repo := withTempProject(t)
	abs := filepath.Join(repo, "internal", "Store", "store.go")
	cases := []struct{ repoTop, in string }{
		{"", tcStoreVariantGo},
		{repo, ""},
		{repo, "."},
		{repo, abs},
	}
	for _, c := range cases {
		if got := foldTargetKey(nil, c.repoTop, c.in); got != c.in {
			t.Errorf("foldTargetKey(%q, %q) = %q; want it unchanged", c.repoTop, c.in, got)
		}
	}
}

// TestFoldTargetKeyCacheAgreesWithDisk: a batch shares one caseCache, so the
// cached answer must equal the uncached one. That parity is the whole safety
// argument for caching the probe at all.
func TestFoldTargetKeyCacheAgreesWithDisk(t *testing.T) {
	repo := withTempProject(t)
	cc := newCaseCache()
	for _, in := range []string{tcStoreVariantGo, tcStoreStoreGo, "internal/Store/NotYetWritten.go"} {
		bare, cached := foldTargetKey(nil, repo, in), foldTargetKey(cc, repo, in)
		if bare != cached {
			t.Errorf("foldTargetKey(%q): uncached %q, cached %q", in, bare, cached)
		}
	}
}

// TestLockCaseVariantSpellingsShareOneKey is the bug's headline case: two
// agents, two spellings, one file. Where the FS folds, the second lock must be
// refused as a conflict; where it does not, both spellings name genuinely
// different files and must stay independently lockable.
func TestLockCaseVariantSpellingsShareOneKey(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	folds := caseVariantFSNote(t, repo)
	if !folds {
		// Give the variant a real file of its own, else the lock is refused
		// for not existing and proves nothing about key independence.
		if err := os.MkdirAll(filepath.Join(repo, "internal", "Store"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "internal", "Store", "store.go"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid())) // durable live holder → hard block
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock on the lowercase spelling failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcStoreVariantGo, "-t", tcIntentWrite}, &out, &errBuf)
	combined := out.String() + errBuf.String()

	if folds {
		if code == 0 {
			t.Fatalf("bob locked %q while alice holds %q — one file, two keys (loto-f8m8): %q", tcStoreVariantGo, tcStoreStoreGo, combined)
		}
		return
	}
	if code != 0 {
		t.Fatalf("on a case-sensitive filesystem %q and %q are two files and must lock independently; exit %d: %q", tcStoreVariantGo, tcStoreStoreGo, code, combined)
	}
}

// TestCheckGateCaseVariantAgreesWithLock: the gate's admission decision must
// reach the same verdict as the lock above, since both read Target.Canonical.
// This is the surface the pre-commit hook consults, so a byte-comparison miss
// here admits a write the lock would have refused.
func TestCheckGateCaseVariantAgreesWithLock(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	folds := caseVariantFSNote(t, repo)
	if !folds {
		if err := os.MkdirAll(filepath.Join(repo, "internal", "Store"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "internal", "Store", "store.go"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcStoreVariantGo}, &out, &bytes.Buffer{})

	if folds {
		if code != 1 {
			t.Fatalf("gate admitted %q against alice's lock on %q; exit %d: %q", tcStoreVariantGo, tcStoreStoreGo, code, out.String())
		}
		if !strings.Contains(out.String(), "kind=lock") {
			t.Errorf("expected a lock-kind deny row: %q", out.String())
		}
		return
	}
	if code != 0 {
		t.Fatalf("gate must admit a genuinely different file on a case-sensitive filesystem; exit %d: %q", code, out.String())
	}
}

// TestClaimCaseVariantPrefixCoversLock: claims overlap by prefix
// (domain.PrefixOverlaps), also a byte comparison, so resolveCLIPrefix needs
// the same fold. A claim typed `internal/Store` must cover the store package.
func TestClaimCaseVariantPrefixCoversLock(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	folds := caseVariantFSNote(t, repo)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdClaim, "internal/Store", "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim on the variant prefix failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcStoreStoreGo}, &out, &bytes.Buffer{})

	if folds {
		if code != 1 {
			t.Fatalf("gate admitted %q under alice's claim on internal/Store; exit %d: %q", tcStoreStoreGo, code, out.String())
		}
		if !strings.Contains(out.String(), "kind=claim") {
			t.Errorf("expected a claim-kind deny row: %q", out.String())
		}
		return
	}
	if code != 0 {
		t.Fatalf("internal/Store is a different prefix on a case-sensitive filesystem; exit %d: %q", code, out.String())
	}
}

// The two doors loto-8soe closes. Both are the same defect — a key that was
// read off the directory entry rather than computed from the caller's spelling
// — and both admitted two agents to one file on a case-folding filesystem.
// #306 closed the third door (two spellings of a file that EXISTS) and left
// these two open; Codex filed them as P1s on that PR.

// TestGateRefusesCaseVariantOfABeaconedPathThatDoesNotExist is door one.
// `loto beacon` announces a write to a file that has not been created yet, so
// the old walk had nothing on disk to resolve the tail against and kept the
// caller's casing: internal/store/New.go and internal/store/new.go were two
// keys and B was admitted onto A's ground.
func TestGateRefusesCaseVariantOfABeaconedPathThatDoesNotExist(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	folds := caseVariantFSNote(t, repo)
	const beaconed, variant = "internal/store/New.go", "internal/store/new.go"

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid())) // durable live holder → hard block
	if code := Run([]string{"beacon", beaconed}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice beacon on %q failed", beaconed)
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, variant}, &out, &bytes.Buffer{})

	if folds {
		if code != 1 {
			t.Fatalf("gate admitted %q against alice's beacon on %q — the key was read off a directory entry that does not exist (loto-8soe); exit %d: %q", variant, beaconed, code, out.String())
		}
		// A beacon is a lock row, so the gate names it kind=lock.
		if !strings.Contains(out.String(), "kind=lock") {
			t.Errorf("expected a lock-kind deny row: %q", out.String())
		}
		if !strings.Contains(out.String(), alice.UUID) {
			t.Errorf("the refusal must name alice as the holder: %q", out.String())
		}
		return
	}
	if code != 0 {
		t.Fatalf("on a case-sensitive filesystem %q and %q are two files; exit %d: %q", beaconed, variant, code, out.String())
	}
}

// TestGateRefusesAfterCaseOnlyRename is door two. The lock's key used to be
// whatever the directory entry was spelled at acquire time, and that spelling
// is mutable: `git mv foo.go Foo.go` moved the key out from under a live lock,
// so the next agent resolving Foo.go byte-matched nothing and was admitted.
func TestGateRefusesAfterCaseOnlyRename(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	folds := caseVariantFSNote(t, repo)
	const lower, upper = "renamed.go", "Renamed.go"
	if err := os.WriteFile(filepath.Join(repo, lower), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, lower, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock on %q failed", lower)
	}

	// The rename anyone can do while the lock is live. On a case-folding
	// filesystem this is one entry changing spelling; on a case-sensitive one
	// it moves the file to a genuinely different path, which is why the two
	// branches below want opposite verdicts.
	if err := os.Rename(filepath.Join(repo, lower), filepath.Join(repo, upper)); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, upper}, &out, &bytes.Buffer{})

	if folds {
		if code != 1 {
			t.Fatalf("gate admitted %q after a case-only rename of alice's locked %q (loto-8soe); exit %d: %q", upper, lower, code, out.String())
		}
		if !strings.Contains(out.String(), alice.UUID) {
			t.Errorf("the refusal must name alice as the holder: %q", out.String())
		}
		return
	}
	if code != 0 {
		t.Fatalf("on a case-sensitive filesystem %q is a different path from the locked %q; exit %d: %q", upper, lower, code, out.String())
	}
}
