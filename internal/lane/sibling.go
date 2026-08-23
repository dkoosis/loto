package lane

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
)

// UnlistedSibling is an untracked file living in the same directory as a
// write-set entry, but absent from the write-set itself.
type UnlistedSibling struct {
	// Path is the untracked file, repo-relative and slash-separated.
	Path string
	// Dir is the shared directory (repo-relative), i.e. path.Dir(Path).
	Dir string
}

// untrackedStatusCode is the two-letter `git status --porcelain=v1` code for a
// file git has never seen — the only code SiblingUntracked matches.
const untrackedStatusCode = "??"

// SiblingUntracked finds untracked files (never `git add`-ed, so absent from
// every commit and from any lane's write-set) that live in the SAME directory
// as a writeSet entry.
//
// This is the cheap half of loto-5aug: `loto lane` commits an exact,
// hand-listed write-set by plumbing — the working tree the author verified
// and the tree the commit records can legitimately differ, that's the whole
// point of a lane. The gap: nothing said so. The incident this reproduces —
// cmd/ferret/main.go registered a command whose body lived in
// cmd/ferret/parallel.go, an untracked leftover from an abandoned branch; the
// lane listed main.go, not parallel.go, and the commit carried a reference to
// ParallelCmd that existed nowhere in its own tree. `make check` on the
// working tree was green and correct; the commit never was.
//
// Scope is deliberately narrow — untracked only, directory-adjacency only, no
// compiler:
//   - Untracked, not "any dirty file in the directory": two lanes routinely
//     edit different EXISTING files in one package concurrently (this repo's
//     whole model — parallel sessions, disjoint write-sets, one shared tree).
//     A peer's in-flight edit to a tracked neighbor never reaches this commit
//     — buildLaneTree seeds the index from the PARENT, so an unlisted tracked
//     file's committed content is the parent's, unaffected by what's dirty on
//     disk. A brand-new file nobody has ever committed is the narrow,
//     high-signal case: it is exactly what an abandoned-branch leftover looks
//     like, and normal concurrent editing of existing files never produces it.
//   - Directory adjacency, not import/symbol analysis: Go's directory-is-package
//     convention makes "same directory" a free, language-native proxy for "same
//     package" with no parse step. It over-warns for a same-dir file the listed
//     files never reference (e.g. an unrelated new scratch file) and
//     under-warns for a cross-package reference — both are the traded-off cost
//     of a check cheap enough to run on every lane; `loto lane --build` is the
//     thorough fallback when that trade isn't good enough.
func SiblingUntracked(ctx context.Context, repoTop string, writeSet []string) ([]UnlistedSibling, error) {
	g := gitRunner{repoTop: repoTop}
	out, err := g.run(ctx, gitCall{args: []string{"status", "--porcelain=v1", "--untracked-files=all", "-z"}})
	if err != nil {
		return nil, fmt.Errorf("lane: sibling scan: %w", err)
	}

	listedDirs := make(map[string]bool, len(writeSet))
	listed := make(map[string]bool, len(writeSet))
	for _, p := range writeSet {
		listed[p] = true
		listedDirs[path.Dir(p)] = true
	}

	var siblings []UnlistedSibling
	for entry := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		// Untracked entries are exactly "?? <path>" — porcelain=v1 has no rename
		// payload for "??" (renames only apply to tracked R/C codes), so no
		// second NUL-delimited field to skip here.
		p, ok := strings.CutPrefix(entry, untrackedStatusCode+" ")
		if !ok || listed[p] {
			continue
		}
		if dir := path.Dir(p); listedDirs[dir] {
			siblings = append(siblings, UnlistedSibling{Path: p, Dir: dir})
		}
	}
	sort.Slice(siblings, func(i, j int) bool { return siblings[i].Path < siblings[j].Path })
	return siblings, nil
}
