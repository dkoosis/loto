package lane

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// sibling_test.go is the executable spec for SiblingUntracked (loto-5aug):
// it warns on a file sharing a directory with a write-set entry that the
// lane commit's tree will NOT carry, and stays silent on the shapes that are
// routine in this repo's shared-tree, disjoint-write-set model. Write-set
// entries here are bare strings passed to the function under test, not files
// written to disk — SiblingUntracked never stats a write-set entry itself, it
// only uses it to derive which directories to inspect and to exclude itself
// from its own findings.
const (
	siblingMainGo     = "cmd/ferret/main.go"
	siblingParallelGo = "cmd/ferret/parallel.go"
	// siblingListedElsewhere is a fabricated write-set entry in a directory the
	// test never touches on disk — SiblingUntracked never stats write-set
	// entries, so it needs no real file behind it.
	siblingListedElsewhere = "pkg/other/listed.go"
	// siblingListedInFerretDir is a fabricated write-set entry sharing
	// cmd/ferret with siblingMainGo/siblingParallelGo, but distinct from
	// either — needed by tests that must detect a real untracked file in that
	// directory without the write-set's own self-match short-circuiting it.
	siblingListedInFerretDir = "cmd/ferret/registered.go"
)

// TestSiblingUntracked_FlagsFileInSameDirNotInWriteSet reproduces the
// incident directly: cmd/ferret/main.go (listed) calls ParallelCmd, defined
// only in cmd/ferret/parallel.go — untracked, never listed.
func TestSiblingUntracked_FlagsFileInSameDirNotInWriteSet(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, siblingParallelGo, "package main\n\nfunc ParallelCmd() {}\n") // never added

	got, err := SiblingUntracked(context.Background(), repoTop, base, []string{siblingMainGo})
	if err != nil {
		t.Fatalf("SiblingUntracked: %v", err)
	}
	if len(got) != 1 || got[0].Path != siblingParallelGo {
		t.Fatalf("want [%s], got %+v", siblingParallelGo, got)
	}
	if got[0].Dir != "cmd/ferret" {
		t.Errorf("Dir = %q, want cmd/ferret", got[0].Dir)
	}
	if got[0].Origin != OriginUntracked {
		t.Errorf("Origin = %v for a never-`git add`-ed file, want OriginUntracked", got[0].Origin)
	}
}

// TestSiblingUntracked_FlagsStagedNewSibling pins codex #286 finding 1: a
// single `git add` on the leftover must not defeat the check. Staging changes
// nothing about whether the lane commit builds — buildLaneTree seeds the
// commit's index from the lane's PARENT COMMIT, never the shared index, so a
// staged-but-uncommitted new file is exactly as absent from the lane commit
// as an untracked one.
func TestSiblingUntracked_FlagsStagedNewSibling(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, siblingParallelGo, "package main\n\nfunc ParallelCmd() {}\n")
	gitT(t, repoTop, "add", siblingParallelGo) // the one-line defeat this pins

	got, err := SiblingUntracked(context.Background(), repoTop, base, []string{siblingMainGo})
	if err != nil {
		t.Fatalf("SiblingUntracked: %v", err)
	}
	if len(got) != 1 || got[0].Path != siblingParallelGo {
		t.Fatalf("staging must not silence the warning: want [%s], got %+v", siblingParallelGo, got)
	}
	// dk review on #286: a staged sibling must be REPORTED as staged, not
	// flattened to the same "untracked" word `git status` would contradict.
	if got[0].Origin != OriginStaged {
		t.Errorf("Origin = %v for a `git add`-ed file, want OriginStaged", got[0].Origin)
	}
}

// TestSiblingUntracked_FlagsFileCommittedAfterParent pins dk's #286
// follow-up (Codex finding on commit 145a550): `git status` reports relative
// to HEAD, not to the lane's parent. `--base main` from a checkout that is
// AHEAD of main is ordinary usage, not contrived — a sibling committed in
// that gap is clean per `git status` (so the round-2 fix's status-only proxy
// missed it) yet absent from the lane parent's tree, so the commit omits it
// exactly like the untracked/staged cases. This is why SiblingUntracked
// answers "is this path in parent's tree?" directly instead of asking git
// status a question status was never positioned to answer.
func TestSiblingUntracked_FlagsFileCommittedAfterParent(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	// Advance HEAD past base with an ordinary, fully committed change that
	// adds a file in the SAME directory a listed file lives in.
	writeFile(t, repoTop, siblingParallelGo, "package main\n\nfunc ParallelCmd() {}\n")
	gitT(t, repoTop, "add", siblingParallelGo)
	gitT(t, repoTop, "commit", "-qm", "add parallel.go (lands on HEAD, not on base)")

	got, err := SiblingUntracked(context.Background(), repoTop, base, []string{siblingMainGo})
	if err != nil {
		t.Fatalf("SiblingUntracked: %v", err)
	}
	if len(got) != 1 || got[0].Path != siblingParallelGo {
		t.Fatalf("a sibling committed after base must still be flagged: want [%s], got %+v", siblingParallelGo, got)
	}
	if got[0].Origin != OriginCommittedAfterParent {
		t.Errorf("Origin = %v, want OriginCommittedAfterParent (git status shows this file clean, it's fully committed at HEAD)", got[0].Origin)
	}
}

// TestSiblingUntracked_SilentOnStagedEditToTrackedFile is the companion guard
// against over-widening: staging a MODIFICATION to an ALREADY-TRACKED file
// ("M " / " M", never "A?") must stay silent — that is the routine
// concurrent-edit-of-an-existing-file shape (a peer's tracked neighbor,
// staged or not), not a new leftover, and it is already present in the
// parent's tree regardless of what's dirty or staged on top of it.
func TestSiblingUntracked_SilentOnStagedEditToTrackedFile(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, "mul.go", mulBroken) // mul.go is tracked (newBaseRepo)
	gitT(t, repoTop, "add", "mul.go")          // now "M " (staged edit), not "A "

	got, err := SiblingUntracked(context.Background(), repoTop, base, []string{siblingListedElsewhere})
	if err != nil {
		t.Fatalf("SiblingUntracked: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no siblings for a staged edit to a tracked file, got %+v", got)
	}
}

// spoofOrigName is a rename source path rigged so that, if a rename's second
// (original-path) status field were ever fed back through the status-code
// check instead of being skipped, entry[0]=='A' would pass the "added" match
// and entry[3:] would slice out "cmd/ferret/spoofed-leak.go" — a fabricated
// path landing in a directory this test's write-set actually touches. That
// makes the spoof observable (an extra, bogus result) rather than silently
// landing in some unrelated directory and going unnoticed either way.
const spoofOrigName = "AXXcmd/ferret/spoofed-leak.go"

// TestSiblingUntracked_RenameOrigPathDoesNotSpoofStatus guards the -z parser:
// a staged rename emits a second NUL-delimited field (the ORIGINAL path)
// immediately after the "R  newpath" entry. That original path must be
// scanned past, not fed back through the status-code check on the next loop
// iteration. (This only affects Origin messaging now, not the verdict — see
// statusOrigins — but a bogus map entry would still misname the row.)
func TestSiblingUntracked_RenameOrigPathDoesNotSpoofStatus(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	// spoofOrigName's real directory is "AXXcmd/ferret" (no slash between the
	// rigged 3-char prefix and "cmd" — the split only happens via entry[3:]
	// slicing, never as a genuine path separator); git mv needs it to exist.
	if err := os.MkdirAll(filepath.Join(repoTop, "AXXcmd", "ferret"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, repoTop, "mv", "mul.go", spoofOrigName)
	gitT(t, repoTop, "commit", "-qm", "rename mul.go to "+spoofOrigName)
	gitT(t, repoTop, "mv", spoofOrigName, "renamed-away.go") // staged rename: orig=spoofOrigName
	writeFile(t, repoTop, siblingMainGo, "package main\n")   // the one real sibling to detect

	got, err := SiblingUntracked(context.Background(), repoTop, base, []string{siblingListedInFerretDir})
	if err != nil {
		t.Fatalf("SiblingUntracked: %v", err)
	}
	if len(got) != 1 || got[0].Path != siblingMainGo {
		t.Fatalf("a rename's orig-path field corrupted parsing: want exactly [%s], got %+v (an extra 'cmd/ferret/spoofed-leak.go' entry means the orig-path field leaked through as a fake status line)", siblingMainGo, got)
	}
}

// TestSiblingUntracked_SilentOnTrackedNeighborEdit is the false-alarm guard:
// a peer lane's in-flight edit to an EXISTING file in the same directory must
// not warn. It is already present in the parent's tree regardless of what's
// dirty on disk — this is the routine shape (parallel sessions, disjoint
// write-sets, one shared tree), not the incident.
func TestSiblingUntracked_SilentOnTrackedNeighborEdit(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	// mul.go is already tracked (newBaseRepo commits it); dirty it in place — a
	// peer lane mid-edit on an existing file, not a new leftover.
	writeFile(t, repoTop, "mul.go", mulBroken)

	got, err := SiblingUntracked(context.Background(), repoTop, base, []string{siblingListedElsewhere})
	if err != nil {
		t.Fatalf("SiblingUntracked: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no siblings for a tracked neighbor edit, got %+v", got)
	}
}

// TestSiblingUntracked_SilentOnUntrackedFileInOtherDir confirms the check is
// directory-scoped, not repo-wide: an untracked file elsewhere in the tree
// (a peer's unrelated new file in a different package) must not warn.
func TestSiblingUntracked_SilentOnUntrackedFileInOtherDir(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, "cmd/other/scratch.go", "package other\n")

	got, err := SiblingUntracked(context.Background(), repoTop, base, []string{siblingListedElsewhere})
	if err != nil {
		t.Fatalf("SiblingUntracked: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no siblings across directories, got %+v", got)
	}
}

// TestSiblingUntracked_IgnoresWriteSetEntryItself confirms an untracked file
// that IS part of the write-set (a brand-new file this lane is adding) is not
// reported against itself.
func TestSiblingUntracked_IgnoresWriteSetEntryItself(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, siblingMainGo, "package main\n")

	got, err := SiblingUntracked(context.Background(), repoTop, base, []string{siblingMainGo})
	if err != nil {
		t.Fatalf("SiblingUntracked: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no self-report, got %+v", got)
	}
}

// TestSiblingUntracked_MultipleUntrackedSortedDeterministic checks output is
// sorted for byte-identical rendering (design.md: deterministic sort).
func TestSiblingUntracked_MultipleUntrackedSortedDeterministic(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, "cmd/ferret/z.go", "package main\n")
	writeFile(t, repoTop, "cmd/ferret/a.go", "package main\n")

	got, err := SiblingUntracked(context.Background(), repoTop, base, []string{siblingMainGo})
	if err != nil {
		t.Fatalf("SiblingUntracked: %v", err)
	}
	if len(got) != 2 || got[0].Path != "cmd/ferret/a.go" || got[1].Path != "cmd/ferret/z.go" {
		t.Fatalf("want sorted [a.go z.go], got %+v", got)
	}
}
