package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Case-variant path regression tests (loto-f8m8).
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
// lookupEntryFold is tested unconditionally so the fold logic itself has
// coverage on every runner.

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
// folding anything. Reading that as "case-insensitive" would let resolveDiskCase
// rewrite A.go to an existing a.go, so `loto lock A.go` would lock a different
// file than the caller named (codex review on #306).
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

// TestLookupEntryFoldPrefersExactOverFolded runs on every filesystem: it feeds
// lookupEntryFold a directory holding both spellings where the FS allows it,
// and asserts the exact match wins so a path that was already right is never
// rewritten.
//
// Run twice — uncached and cached — so the batch cache cannot answer differently
// from disk. That parity is the whole safety argument for caching at all.
func TestLookupEntryFoldPrefersExactOverFolded(t *testing.T) {
	for _, tc := range []struct {
		name string
		cc   *caseCache
	}{{"uncached", nil}, {"cached", newCaseCache()}} {
		t.Run(tc.name, func(t *testing.T) { lookupEntryFoldChecks(t, tc.cc) })
	}
}

func lookupEntryFoldChecks(t *testing.T, cc *caseCache) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "Store"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A second, lowercase entry only exists on a case-sensitive FS; where it
	// collides the Mkdir fails and the exact-match leg below is vacuous, which
	// is the correct behavior for that FS.
	lowerExists := os.Mkdir(filepath.Join(dir, "store"), 0o755) == nil

	got, ok := lookupEntryFold(cc, dir, "Store")
	if !ok || got != "Store" {
		t.Errorf("lookupEntryFold(%q, \"Store\") = %q,%v; want \"Store\",true", dir, got, ok)
	}
	if lowerExists {
		got, ok = lookupEntryFold(cc, dir, "store")
		if !ok || got != "store" {
			t.Errorf("exact match must win over fold: got %q,%v; want \"store\",true", got, ok)
		}
	} else {
		// Case-insensitive FS: only "Store" is on disk, so the folded lookup
		// must report the on-disk spelling for the lowercase request.
		got, ok = lookupEntryFold(cc, dir, "store")
		if !ok || got != "Store" {
			t.Errorf("folded lookup = %q,%v; want \"Store\",true", got, ok)
		}
	}

	if _, ok := lookupEntryFold(cc, dir, "absent"); ok {
		t.Error("lookupEntryFold reported a hit for an entry that does not exist")
	}
	if _, ok := lookupEntryFold(cc, filepath.Join(dir, "no-such-dir"), "x"); ok {
		t.Error("lookupEntryFold reported a hit under an unreadable directory")
	}
}

// TestResolveDiskCaseFoldsDirectorySpelling: the canonical key a variant
// spelling produces must be the on-disk spelling where the FS folds, and must
// be left alone where it does not.
func TestResolveDiskCaseFoldsDirectorySpelling(t *testing.T) {
	repo := withTempProject(t) // creates internal/store/store.go

	got := resolveDiskCase(nil, repo, tcStoreVariantGo)
	if caseVariantFSNote(t, repo) {
		if got != tcStoreStoreGo {
			t.Errorf("resolveDiskCase(%q) = %q; want %q — the variant must fold to the on-disk spelling", tcStoreVariantGo, got, tcStoreStoreGo)
		}
		return
	}
	if got != tcStoreVariantGo {
		t.Errorf("resolveDiskCase(%q) = %q; want it unchanged on a case-sensitive filesystem", tcStoreVariantGo, got)
	}
}

// TestResolveDiskCaseKeepsUnknownTail: a beacon names a file that does not
// exist yet, so disk has no truth to offer about its segment. Directory case
// still converges; the typed tail survives verbatim.
func TestResolveDiskCaseKeepsUnknownTail(t *testing.T) {
	repo := withTempProject(t)
	got := resolveDiskCase(nil, repo, "internal/Store/NotYetWritten.go")
	want := "internal/Store/NotYetWritten.go"
	if caseVariantFSNote(t, repo) {
		want = "internal/store/NotYetWritten.go"
	}
	if got != want {
		t.Errorf("resolveDiskCase = %q; want %q", got, want)
	}
}

// TestResolveDiskCaseLeavesUnwalkableShapes covers the early returns: an empty
// repo frame, ".", an absolute path, and a relative token carrying "..". None
// is a shape the segment walk can reason about.
func TestResolveDiskCaseLeavesUnwalkableShapes(t *testing.T) {
	repo := withTempProject(t)
	abs := filepath.Join(repo, "internal", "Store", "store.go")
	cases := []struct{ repoTop, in string }{
		{"", tcStoreVariantGo},
		{repo, ""},
		{repo, "."},
		{repo, abs},
		{repo, "internal/../" + tcStoreVariantGo},
	}
	for _, c := range cases {
		if got := resolveDiskCase(nil, c.repoTop, c.in); got != c.in {
			t.Errorf("resolveDiskCase(%q, %q) = %q; want it unchanged", c.repoTop, c.in, got)
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
