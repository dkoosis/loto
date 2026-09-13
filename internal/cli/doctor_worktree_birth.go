package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// worktreeBirthStaleAfter is how long a worktree admin dir may sit mid-birth
// before doctor calls it cruft rather than an in-flight `git worktree add`.
// Measured (loto-w0sx phase 1, git 2.55.0): a live birth holds this shape for
// milliseconds. Nobody has measured the slow end — a huge checkout, a wedged
// disk — so this is a conservative multiple of that floor, not a measured
// ceiling (loto-rode Questions: open). Cheap to retune; nothing else in the
// guard or doctor depends on the exact value.
const worktreeBirthStaleAfter = 5 * time.Minute

// staleUnbornWorktree is one worktree admin dir doctor considers cruft: it
// has been mid-birth (worktreeUnborn, cmd_hook_ref.go) longer than
// worktreeBirthStaleAfter.
type staleUnbornWorktree struct {
	Dir      string // admin dir name under the common dir's worktrees/
	AdminDir string // absolute path to that admin dir, in repoTop's own frame
	RelDir   string // AdminDir relative to repoTop, for display
	Branch   string // target branch name, "unknown" if HEAD.lock unreadable
	Age      time.Duration
}

// scanStaleUnbornWorktrees lists every worktree admin dir under repoTop's
// common dir that is unborn (worktreeUnborn, shared with the ref guard's
// headWorktreeBirth carve-out, loto-w0sx) AND has held that shape longer than
// threshold. A fresh mid-birth dir — a `git worktree add` genuinely in flight
// — is never reported: Rules forbid treating a live birth as cruft.
//
// A git failure (no git, not a repo, no worktrees dir) is swallowed to nil
// rather than surfaced as a doctor error — this report is advisory, like
// scanDanglingStashes beside it, never load-bearing for doctor's exit code.
func scanStaleUnbornWorktrees(ctx context.Context, repoTop string, now time.Time, threshold time.Duration) []staleUnbornWorktree {
	if repoTop == "" {
		return nil
	}
	common, err := gitCmd(ctx, repoTop, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil
	}
	commonDir := strings.TrimSpace(common)
	if !filepath.IsAbs(commonDir) {
		// git reports this relative to repoTop (the common case: repoTop is a
		// plain checkout, common dir is its own .git). Joining onto repoTop —
		// rather than asking git for the absolute form — keeps this in the
		// SAME path frame relPath's os.Getwd() uses: on a filesystem where
		// $TMPDIR resolves through a symlink (macOS `/var` -> `/private/var`),
		// git's own absolute/physical form and os.Getwd()'s logical form name
		// the identical directory with two different strings, and relPath
		// would silently fall back to printing the absolute path.
		commonDir = filepath.Join(repoTop, commonDir)
	}
	absRepoTop, err := filepath.Abs(repoTop)
	if err != nil {
		absRepoTop = repoTop
	}
	entries, err := os.ReadDir(filepath.Join(commonDir, refWorktreesDir))
	if err != nil {
		return nil
	}
	var out []staleUnbornWorktree
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(commonDir, refWorktreesDir, e.Name())
		if !worktreeUnborn(dir) {
			continue
		}
		lockInfo, err := os.Stat(filepath.Join(dir, refHeadLockFile))
		if err != nil {
			// No HEAD.lock to date it by — nothing left to report age against.
			continue
		}
		age := now.Sub(lockInfo.ModTime())
		if age < threshold {
			continue
		}
		out = append(out, staleUnbornWorktree{
			Dir:      e.Name(),
			AdminDir: dir,
			RelDir:   relToBase(absRepoTop, dir),
			Branch:   worktreeBirthTargetBranch(dir),
			Age:      age,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

// relToBase returns target relative to base, falling back to target
// unchanged when they can't be related (different volume, or the relative
// form would escape base with "../"). Deliberately NOT relPath/os.Getwd(): on
// a filesystem where $TMPDIR resolves through a symlink (macOS `/var` ->
// `/private/var`), git's physically-resolved absolute paths and os.Getwd()'s
// logical one name the same directory with two different strings, and
// relPath would silently fall back to the absolute form. base and target
// here both derive from the same repoTop string, so they share one frame
// regardless of symlinks.
func relToBase(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return target
	}
	return rel
}

// worktreeBirthTargetBranch reads the branch name a stale HEAD.lock was about
// to point HEAD at, trimmed of the refs/heads/ prefix for display, via the
// same refSymrefTarget cmd_hook_ref.go parses HEAD.lock content with.
// "unknown" when the file is unreadable or does not name a branch (a
// detached-HEAD birth carries none).
func worktreeBirthTargetBranch(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, refHeadLockFile))
	if err != nil {
		return unknownBuildField
	}
	target := refSymrefTarget(strings.TrimSpace(string(raw)))
	branch := strings.TrimPrefix(target, refHeadsPrefix)
	if branch == "" {
		return unknownBuildField
	}
	return branch
}

// renderStaleUnbornWorktrees prints one ⚠ row per stale unborn worktree admin
// dir — advisory, per design.md's severity glyphs, and never touched by
// --repair beside it (loto-rode Rules: reaping is never automatic) — naming
// the dir, its target branch, its age, and the manual clear. Silent at zero
// so a checkout with none keeps doctor's existing byte-identical output.
func renderStaleUnbornWorktrees(w io.Writer, stales []staleUnbornWorktree) {
	if len(stales) == 0 {
		return
	}
	fmt.Fprintf(w, "⚠ stale_unborn_worktrees count=%d\n", len(stales))
	for _, s := range stales {
		fmt.Fprintf(w, "⚠ worktree dir=%s branch=%s age=%s — mid-birth longer than a `git worktree add` should take; clear it:\n",
			s.RelDir, s.Branch, s.Age.Round(time.Second))
		fmt.Fprintln(w, "```bash")
		fmt.Fprintf(w, "rm -rf %s && git worktree prune\n", s.RelDir)
		fmt.Fprintln(w, "```")
	}
}
