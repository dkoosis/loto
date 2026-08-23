package lane

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
)

// UnlistedSibling is a file new to the repo's history — absent from every
// commit, whether or not it has since been `git add`-ed — living in the same
// directory as a write-set entry, but absent from the write-set itself.
type UnlistedSibling struct {
	// Path is the file, repo-relative and slash-separated.
	Path string
	// Dir is the shared directory (repo-relative), i.e. path.Dir(Path).
	Dir string
	// Staged is true when the file is already `git add`-ed (git status "A "),
	// false when it is merely untracked ("??"). Both are equally invisible to
	// buildLaneTree's parent-seeded commit — the distinction changes nothing
	// about the risk — but it changes what the reader does next: staged means
	// list it; untracked means stage-or-delete it, then list it. A report
	// that calls a staged file "untracked" sends the reader chasing a git
	// status mismatch instead of the actual fix.
	Staged bool
}

// SiblingUntracked finds files new to the repo's history — never committed,
// whether or not they have since been `git add`-ed — that live in the SAME
// directory as a writeSet entry.
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
// Scope is deliberately narrow — new-to-history only, directory-adjacency
// only, no compiler:
//   - New to history ("??" untracked OR "A?" staged-new), not "any dirty file
//     in the directory": two lanes routinely edit different EXISTING,
//     already-committed files in one package concurrently (this repo's whole
//     model — parallel sessions, disjoint write-sets, one shared tree). A
//     peer's in-flight edit to a tracked neighbor never reaches this commit —
//     buildLaneTree seeds the index from the PARENT COMMIT, so an unlisted
//     tracked file's committed content is the parent's, unaffected by what's
//     dirty on disk or staged in the shared index. Staging the leftover with
//     `git add` changes nothing about that: a staged-but-uncommitted NEW file
//     is exactly as absent from buildLaneTree's parent-seeded tree as an
//     untracked one, so "A" (added-to-index) is matched right alongside "??"
//     (codex #286 finding 1 — a single `git add` on the leftover used to
//     silence this check with no change to whether the commit builds). "M" /
//     an "M" on an already-tracked file's Y side stays excluded, deliberately
//     — that is the routine concurrent-edit-of-an-existing-file shape above,
//     not a brand-new leftover, and warning on it would fire constantly in
//     this repo's normal operating mode.
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

	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var siblings []UnlistedSibling
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 3 {
			continue
		}
		x, y := entry[0], entry[1]
		// A rename/copy entry carries a second NUL-delimited field (the
		// original path) immediately after this one in -z output — consume
		// and skip it. Neither code this function matches ("??", "A?") is
		// ever a rename/copy, so this branch only ever fires for entries this
		// loop would skip anyway; it exists so a rename's ORIGINAL path can
		// never be misread as its own status line on the next iteration (an
		// old path starting with 'A' would otherwise spoof "added").
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			i++
			continue
		}
		untracked := entry[:2] == "??"
		staged := x == 'A'
		if !untracked && !staged {
			continue
		}
		p := entry[3:]
		if listed[p] {
			continue
		}
		if dir := path.Dir(p); listedDirs[dir] {
			siblings = append(siblings, UnlistedSibling{Path: p, Dir: dir, Staged: staged})
		}
	}
	sort.Slice(siblings, func(i, j int) bool { return siblings[i].Path < siblings[j].Path })
	return siblings, nil
}
