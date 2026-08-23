package lane

import (
	"context"
	"testing"
)

// sibling_test.go is the executable spec for SiblingUntracked (loto-5aug):
// it warns on an untracked file sharing a directory with a write-set entry,
// and stays silent on the shapes that are routine in this repo's
// shared-tree, disjoint-write-set model. Write-set entries here are bare
// strings passed to the function under test, not files written to disk —
// SiblingUntracked never stats them, it only compares directories against
// git status output, so no on-disk file is needed to represent "listed".
const (
	siblingMainGo     = "cmd/ferret/main.go"
	siblingParallelGo = "cmd/ferret/parallel.go"
	// siblingListedElsewhere is a fabricated write-set entry in a directory the
	// test never touches on disk — SiblingUntracked never stats write-set
	// entries, so it needs no real file behind it.
	siblingListedElsewhere = "pkg/other/listed.go"
)

// TestSiblingUntracked_FlagsFileInSameDirNotInWriteSet reproduces the
// incident directly: cmd/ferret/main.go (listed) calls ParallelCmd, defined
// only in cmd/ferret/parallel.go — untracked, never listed.
func TestSiblingUntracked_FlagsFileInSameDirNotInWriteSet(t *testing.T) {
	repoTop, _ := newBaseRepo(t)
	writeFile(t, repoTop, siblingParallelGo, "package main\n\nfunc ParallelCmd() {}\n") // never added

	got, err := SiblingUntracked(context.Background(), repoTop, []string{siblingMainGo})
	if err != nil {
		t.Fatalf("SiblingUntracked: %v", err)
	}
	if len(got) != 1 || got[0].Path != siblingParallelGo {
		t.Fatalf("want [%s], got %+v", siblingParallelGo, got)
	}
	if got[0].Dir != "cmd/ferret" {
		t.Errorf("Dir = %q, want cmd/ferret", got[0].Dir)
	}
}

// TestSiblingUntracked_SilentOnTrackedNeighborEdit is the false-alarm guard:
// a peer lane's in-flight edit to an EXISTING file in the same directory must
// not warn. buildLaneTree seeds the commit's index from the parent, so an
// unlisted tracked file's committed content is the parent's regardless of
// what's dirty on disk — this is the routine shape (parallel sessions,
// disjoint write-sets, one shared tree), not the incident.
func TestSiblingUntracked_SilentOnTrackedNeighborEdit(t *testing.T) {
	repoTop, _ := newBaseRepo(t)
	// mul.go is already tracked (newBaseRepo commits it); dirty it in place — a
	// peer lane mid-edit on an existing file, not a new leftover.
	writeFile(t, repoTop, "mul.go", mulBroken)

	got, err := SiblingUntracked(context.Background(), repoTop, []string{siblingListedElsewhere})
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
	repoTop, _ := newBaseRepo(t)
	writeFile(t, repoTop, "cmd/other/scratch.go", "package other\n")

	got, err := SiblingUntracked(context.Background(), repoTop, []string{siblingListedElsewhere})
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
	repoTop, _ := newBaseRepo(t)
	writeFile(t, repoTop, siblingMainGo, "package main\n")

	got, err := SiblingUntracked(context.Background(), repoTop, []string{siblingMainGo})
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
	repoTop, _ := newBaseRepo(t)
	writeFile(t, repoTop, "cmd/ferret/z.go", "package main\n")
	writeFile(t, repoTop, "cmd/ferret/a.go", "package main\n")

	got, err := SiblingUntracked(context.Background(), repoTop, []string{siblingMainGo})
	if err != nil {
		t.Fatalf("SiblingUntracked: %v", err)
	}
	if len(got) != 2 || got[0].Path != "cmd/ferret/a.go" || got[1].Path != "cmd/ferret/z.go" {
		t.Fatalf("want sorted [a.go z.go], got %+v", got)
	}
}
