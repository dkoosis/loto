package lane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SiblingOrigin describes HOW a flagged sibling reached the working tree.
// It is for MESSAGING only — which git command explains what the reader is
// looking at, and what they'd do next — never for deciding whether to warn.
// That verdict is answered directly by SiblingUntracked: is this path absent
// from BOTH the write-set and the lane's parent tree.
type SiblingOrigin int

const (
	// OriginUntracked: never `git add`-ed.
	OriginUntracked SiblingOrigin = iota
	// OriginStaged: `git add`-ed, not yet committed.
	OriginStaged
	// OriginCommittedAfterParent: fully committed at HEAD, but the lane's
	// parent (Base, or an earlier wave's tip) predates that commit — this
	// checkout is ahead of the tree the lane forks from.
	OriginCommittedAfterParent
)

// UnlistedSibling is a file, living in the same directory as a write-set
// entry, that the lane commit will NOT carry: present in the working tree,
// absent from both the write-set and the lane's parent tree.
type UnlistedSibling struct {
	// Path is the file, repo-relative and slash-separated.
	Path string
	// Dir is the shared directory (repo-relative), i.e. path.Dir(Path).
	Dir string
	// Origin is messaging-only — see SiblingOrigin.
	Origin SiblingOrigin
}

// SiblingUntracked finds files the lane commit's tree will NOT carry: present
// in the working tree, in the SAME directory as a writeSet entry, but neither
// listed in writeSet nor present in parent's tree at that path. parent is the
// commit-ish the lane actually forks from — in the CLI pipeline, the just-made
// commit's own parent (`<commit>^`), which is exactly what buildLaneTree
// seeded the staged index from, whether that came from Base or an earlier
// wave's tip.
//
// This is the cheap half of loto-5aug: `loto lane` commits an exact,
// hand-listed write-set by plumbing — the working tree the author verified
// and the tree the commit records can legitimately differ, that's the whole
// point of a lane. The gap: nothing said so. The incident this reproduces —
// cmd/ferret/main.go registered a command whose body lived in
// cmd/ferret/parallel.go, a leftover from an abandoned branch; the lane
// listed main.go, not parallel.go, and the commit carried a reference to
// ParallelCmd that existed nowhere in its own tree. `make check` on the
// working tree was green and correct; the commit never was.
//
// The question this function answers is "will the commit carry this file?" —
// answered directly, not by a proxy. Two rounds of review each found a gap in
// an earlier proxy formulation:
//   - `git status` alone (round 1, codex #286 finding 1): matched only
//     untracked ("??") files — missed the SAME leftover the moment it was
//     `git add`-ed, because staging removes it from status's untracked
//     bucket even though buildLaneTree still never sees it (it seeds from the
//     PARENT COMMIT, never the shared index).
//   - `git status` widened to "??"+"A?" (round 2, dk review on #286): still
//     missed a file fully COMMITTED at HEAD but postdating the lane's own
//     parent — `--base main` from a checkout ahead of main is ordinary usage.
//     git status reports relative to HEAD/index, not Base, so a
//     HEAD-committed file is invisible to it regardless of what Base is.
//
// Both gaps close under the direct question: does this on-disk file exist in
// parent's tree? A file the working tree gained since parent — whether
// untracked, staged, or fully committed at HEAD — is equally absent from
// parent's tree, and that absence is exactly what buildLaneTree's
// parent-seeded index means for the commit. Directory adjacency stays the
// scope boundary (Go's directory-is-package convention, no compiler): a
// file ALREADY in parent's tree is invisible to this check by construction —
// a peer's edit to it never reaches this commit either, matching the routine
// concurrent-edit-of-an-existing-file shape this repo's shared-tree model
// runs on constantly.
func SiblingUntracked(ctx context.Context, repoTop, parent string, writeSet []string) ([]UnlistedSibling, error) {
	g := gitRunner{repoTop: repoTop}

	listed := make(map[string]bool, len(writeSet))
	dirs := make(map[string]bool, len(writeSet))
	for _, p := range writeSet {
		listed[p] = true
		dirs[path.Dir(p)] = true
	}

	origins, err := statusOrigins(ctx, g)
	if err != nil {
		return nil, fmt.Errorf("lane: sibling scan: status: %w", err)
	}

	var siblings []UnlistedSibling
	for dir := range dirs {
		found, derr := g.siblingsInDir(ctx, repoTop, parent, dir, listed, origins)
		if derr != nil {
			return nil, derr
		}
		siblings = append(siblings, found...)
	}
	sort.Slice(siblings, func(i, j int) bool { return siblings[i].Path < siblings[j].Path })
	return siblings, nil
}

// siblingsInDir does the per-directory work SiblingUntracked's loop body used
// to inline: read the working tree's actual listing, read parent's tree at
// the same directory, and flag every on-disk file present in neither dir's
// listing (write-set) nor parent's tree. Split out to keep SiblingUntracked's
// own cognitive complexity in budget — this function is the whole "will the
// commit carry this file?" check for one directory.
func (g gitRunner) siblingsInDir(ctx context.Context, repoTop, parent, dir string, listed map[string]bool, origins map[string]SiblingOrigin) ([]UnlistedSibling, error) {
	entries, derr := os.ReadDir(filepath.Join(repoTop, filepath.FromSlash(dir)))
	if derr != nil {
		if errors.Is(derr, os.ErrNotExist) {
			return nil, nil // a listed file's directory may not exist on disk (e.g. a pure deletion)
		}
		return nil, fmt.Errorf("lane: sibling scan: read dir %s: %w", dir, derr)
	}
	inParent, terr := g.parentTreeFiles(ctx, parent, dir)
	if terr != nil {
		return nil, fmt.Errorf("lane: sibling scan: ls-tree %s: %w", dir, terr)
	}
	var siblings []UnlistedSibling
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := path.Join(dir, e.Name())
		if listed[p] || inParent[p] {
			continue
		}
		origin, ok := origins[p]
		if !ok {
			// Clean per `git status`: already fully committed at HEAD, just
			// absent from the OLDER parent tree.
			origin = OriginCommittedAfterParent
		}
		siblings = append(siblings, UnlistedSibling{Path: p, Dir: dir, Origin: origin})
	}
	return siblings, nil
}

// parentTreeFiles returns the set of repo-relative file paths git tree parent
// carries DIRECTLY inside dir — one level, matching Go's directory-is-package
// flatness; a subdirectory appears as its own tree entry, never descended
// into. dir == "." lists the tree's root. The trailing "/" on a non-root dir
// is significant to `git ls-tree`: it addresses dir's OWN subtree rather than
// the single tree-typed entry named dir in ITS parent directory's listing.
func (g gitRunner) parentTreeFiles(ctx context.Context, parent, dir string) (map[string]bool, error) {
	args := []string{"ls-tree", "-z", "--name-only", parent}
	if dir != "." {
		args = append(args, "--", dir+"/")
	}
	out, err := g.run(ctx, gitCall{args: args})
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool)
	for n := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		if n != "" {
			names[n] = true
		}
	}
	return names, nil
}

// statusOrigins returns, for every untracked or staged-new path `git status`
// currently reports, which of the two it is — messaging only (SiblingOrigin
// doc). A path SiblingUntracked flags that is absent from this map is
// inferred OriginCommittedAfterParent: clean per status, so already fully
// committed at HEAD.
func statusOrigins(ctx context.Context, g gitRunner) (map[string]SiblingOrigin, error) {
	out, err := g.run(ctx, gitCall{args: []string{"status", "--porcelain=v1", "--untracked-files=all", "-z"}})
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	origins := make(map[string]SiblingOrigin)
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 3 {
			continue
		}
		x, y := entry[0], entry[1]
		// A rename/copy entry carries a second NUL-delimited field (the
		// original path) immediately after this one — consume and skip it.
		// codex #286: an old path starting with 'A' or matching "??" could
		// otherwise spoof a status code on the next loop iteration.
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			i++
			continue
		}
		switch {
		case entry[:2] == "??":
			origins[entry[3:]] = OriginUntracked
		case x == 'A':
			origins[entry[3:]] = OriginStaged
		}
	}
	return origins, nil
}
