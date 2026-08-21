package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scanRepo is newIntegrationRepo with refs/loto/integration actually set — the
// scan's baseline. The working tree is checked out at that commit, which is
// what makes "differs from integration" a meaningful reading of it.
// tfFileSorted are three tracked files whose git-diff order is not their
// sorted order — the fixture the path-sort assertion needs.
const tfFileSorted0, tfFileSorted1, tfFileSorted2 = "a2.go", "m.go", "z.go"

func scanRepo(t *testing.T) string {
	t.Helper()
	repoTop, integration := newIntegrationRepo(t)
	gitT(t, repoTop, "update-ref", IntegrationRef, integration)
	return repoTop
}

func mustScan(t *testing.T, repoTop string) []Observation {
	t.Helper()
	obs, err := ScanWorktree(context.Background(), repoTop)
	if err != nil {
		t.Fatalf("ScanWorktree: %v", err)
	}
	return obs
}

// A clean tree is not silence-because-broken; it is a positive empty reading.
func TestScanWorktree_CleanTreeObservesNothing(t *testing.T) {
	if obs := mustScan(t, scanRepo(t)); len(obs) != 0 {
		t.Fatalf("clean tree observed %d changes, want 0: %v", len(obs), obs)
	}
}

// The scripted demo from git-gate.md: a rogue in-place edit of a tracked file.
func TestScanWorktree_RogueEditIsObservedWithItsBlobSHA(t *testing.T) {
	repoTop := scanRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 99 // rogue\n")

	obs := mustScan(t, repoTop)
	if len(obs) != 1 {
		t.Fatalf("want 1 observation, got %d: %v", len(obs), obs)
	}
	if obs[0].Path != tfFileA || obs[0].Deleted {
		t.Fatalf("observation = %+v, want a non-deleted %s", obs[0], tfFileA)
	}
	// The fingerprint must be the content's real blob SHA, not a placeholder:
	// a violation row that cannot be checked against the tree is forensics
	// nobody can use.
	want := gitT(t, repoTop, "hash-object", "--", tfFileA)
	if obs[0].Fingerprint != want {
		t.Errorf("fingerprint = %s, want %s", obs[0].Fingerprint, want)
	}
}

// A deletion mutates integration's content as surely as an edit does.
func TestScanWorktree_DeletionIsObservedWithoutAFingerprint(t *testing.T) {
	repoTop := scanRepo(t)
	if err := os.Remove(filepath.Join(repoTop, tfFileA)); err != nil {
		t.Fatal(err)
	}

	obs := mustScan(t, repoTop)
	if len(obs) != 1 {
		t.Fatalf("want 1 observation, got %d: %v", len(obs), obs)
	}
	if !obs[0].Deleted || obs[0].Fingerprint != "" {
		t.Errorf("observation = %+v, want Deleted with no fingerprint", obs[0])
	}
}

// Untracked files are out of scope by design — absent from integration, so
// "differs from it" says nothing about them (loto-ovno.13 owns that class).
func TestScanWorktree_UntrackedFileIsNotObserved(t *testing.T) {
	repoTop := scanRepo(t)
	writeFile(t, repoTop, "scratch.txt", "notes\n")

	if obs := mustScan(t, repoTop); len(obs) != 0 {
		t.Fatalf("untracked file observed as a change: %v", obs)
	}
}

// The sensor never bootstraps refs/loto/integration — a scan fired from a
// PreToolUse hook has no business writing a ref. No baseline, no reading —
// but that is reported as ErrNoBaseline, NOT as a silent (nil, nil), because
// a caller that cannot tell "no baseline" apart from "compared, found
// nothing" would auto-resolve every violation already on the books the
// moment the ref went missing (Codex #276 P1).
func TestScanWorktree_NoIntegrationRefIsReportedDistinctly(t *testing.T) {
	repoTop, _ := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 99\n")

	obs, err := ScanWorktree(context.Background(), repoTop)
	if !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("want ErrNoBaseline, got obs=%v err=%v", obs, err)
	}
	if len(obs) != 0 {
		t.Fatalf("want no observations without a baseline, got %v", obs)
	}
	if out := gitT(t, repoTop, "for-each-ref", "--format=%(refname)", IntegrationRef); out != "" {
		t.Fatalf("scan created %s — it must never write a ref", out)
	}
}

// A tracked symlink retargeted to a destination that does not exist must not
// take the whole batch down: hash-object --stdin-paths opens the path it is
// given, which for a symlink means following it, and a dangling target makes
// that open fail — for every path in the same call, not just this one
// (Codex #276 P2). The fingerprinter hashes the link's own payload instead,
// so an unrelated rogue edit in the same scan is still recorded.
func TestScanWorktree_DanglingSymlinkIsHashedNotDereferenced(t *testing.T) {
	const link = "link.txt"
	repoTop := scanRepo(t)
	if err := os.Symlink("target-that-exists.txt", filepath.Join(repoTop, link)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repoTop, "target-that-exists.txt", "hi\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "add symlink")
	gitT(t, repoTop, "update-ref", IntegrationRef, "HEAD")

	if err := os.Remove(filepath.Join(repoTop, link)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nonexistent-target.txt", filepath.Join(repoTop, link)); err != nil {
		t.Fatal(err)
	}
	// An ordinary rogue edit alongside it — proves the WHOLE batch survives,
	// not just the symlink.
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")

	// git's own blob id for the retargeted link, straight from the index —
	// the fingerprint we compute has to BE this, not merely be non-empty,
	// or a later scan could not tell one link target from another.
	gitT(t, repoTop, "add", link)
	wantSHA := strings.Fields(gitT(t, repoTop, "ls-files", "-s", link))[1]

	obs := mustScan(t, repoTop)
	var sawLink, sawFile bool
	for _, o := range obs {
		switch o.Path {
		case link:
			sawLink = true
			if o.Fingerprint != wantSHA {
				t.Errorf("symlink fingerprint = %q, want git's own %q", o.Fingerprint, wantSHA)
			}
		case tfFileA:
			sawFile = true
		}
	}
	if !sawLink || !sawFile {
		t.Fatalf("want both %s and %s observed, got %v", link, tfFileA, obs)
	}
}

// Same input, byte-identical output (.claude/rules/design.md): observations
// are path-sorted regardless of how git happened to order the diff.
func TestScanWorktree_ObservationsArePathSorted(t *testing.T) {
	repoTop := scanRepo(t)
	for _, rel := range []string{tfFileSorted2, tfFileSorted1, tfFileSorted0} {
		writeFile(t, repoTop, rel, "package gate\n")
	}
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "more files")
	gitT(t, repoTop, "update-ref", IntegrationRef, gitT(t, repoTop, "rev-parse", "HEAD"))
	for _, rel := range []string{tfFileSorted2, tfFileSorted1, tfFileSorted0} {
		writeFile(t, repoTop, rel, "package gate\n\n// touched\n")
	}

	obs := mustScan(t, repoTop)
	got := make([]string, len(obs))
	for i, o := range obs {
		got[i] = o.Path
	}
	want := []string{tfFileSorted0, tfFileSorted1, tfFileSorted2}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paths = %v, want %v", got, want)
		}
	}
}

// A rename must report as delete+add, not as one R record: the destination is
// exactly the path a candidate would go on to declare, and hiding it there
// would leave the intersect check blind to the contaminated half.
func TestScanWorktree_RenameReportsBothPaths(t *testing.T) {
	repoTop := scanRepo(t)
	gitT(t, repoTop, "mv", tfFileA, "renamed.go")

	obs := mustScan(t, repoTop)
	if len(obs) != 2 {
		t.Fatalf("want both sides of the rename, got %d: %v", len(obs), obs)
	}
	byPath := map[string]Observation{}
	for _, o := range obs {
		byPath[o.Path] = o
	}
	if src, ok := byPath[tfFileA]; !ok || !src.Deleted {
		t.Errorf("source %s = %+v, want observed as deleted", tfFileA, src)
	}
	if dst, ok := byPath["renamed.go"]; !ok || dst.Deleted {
		t.Errorf("destination renamed.go = %+v, want observed as present", dst)
	}
}
