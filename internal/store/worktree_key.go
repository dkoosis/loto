package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// WorktreeMain is the worktree key of a repo's primary checkout.
const WorktreeMain = "main"

// worktreesDir is the directory in the common git dir that holds each linked
// checkout's admin dir.
const worktreesDir = "worktrees"

// WorktreeKey names the checkout rooted at top by its admin dir within the
// repo's common git dir (docs/write-gate.md, "worktree"): "main" for the
// primary checkout, "worktrees/<name>" for a linked one. A hold's worktree
// is this key, not the checkout's path, so `git worktree move`, which keeps
// the admin dir and rewrites only its gitdir pointer, keeps every hold
// (loto-hu8o).
//
// It reads top/.git and nothing else: a directory is the primary checkout; a
// file's "gitdir: <dir>" names the admin dir, which is a linked checkout's
// when it sits in a worktrees/ dir and carries a commondir file. Any other
// gitdir file (a submodule's own checkout) is that repo's primary. "" means
// the layout could not be read: domain.SameWorktree reads it as unknown,
// which can only widen a conflict.
//
// ‡ Unique within one clone. Two clones sharing one store (no LOTO_BASE)
// both key their primary as "main"; that is out of frame for the reasons on
// Store.repoTop (loto-n7xb), and it errs toward more conflicts, not fewer.
func WorktreeKey(top string) string {
	if top == "" {
		return ""
	}
	dotGit := filepath.Join(top, ".git")
	fi, err := os.Stat(dotGit)
	if err != nil {
		return ""
	}
	if fi.IsDir() {
		return WorktreeMain
	}
	b, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	gitDir = filepath.Clean(strings.TrimSpace(gitDir))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(top, gitDir)
	}
	if filepath.Base(filepath.Dir(gitDir)) != worktreesDir {
		return WorktreeMain
	}
	if _, err := os.Stat(filepath.Join(gitDir, "commondir")); err != nil {
		return WorktreeMain
	}
	return worktreesDir + "/" + filepath.Base(gitDir)
}

// ensureHoldsKeyedByAdminDir rewrites every lock and claim row still stamped
// with an absolute checkout path (loto-3eq6 through loto-in0v) to that
// checkout's WorktreeKey (loto-hu8o). A path that no longer holds a checkout
// (moved or removed before this ran) cannot be keyed, so its rows become ""
// — unknown, which conflicts in every worktree and which its owner can still
// release — rather than inert rows no checkout would ever match.
//
// A rewrite that collides with a row already keyed (same target and owner,
// one path and one key naming one checkout) keeps that row and drops the
// stamped one. Claims key on (path_prefix, owner_uuid) alone, so they cannot
// collide.
func ensureHoldsKeyedByAdminDir(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	paths, err := stampedCheckoutPaths(ctx, db)
	if err != nil || len(paths) == 0 {
		return false, err
	}
	if !apply {
		return true, nil
	}
	for _, p := range paths {
		key := WorktreeKey(p)
		if _, err := db.ExecContext(ctx, `UPDATE OR IGNORE locks SET worktree = ? WHERE worktree = ?`, key, p); err != nil {
			return false, err
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM locks WHERE worktree = ?`, p); err != nil {
			return false, err
		}
		if _, err := db.ExecContext(ctx, `UPDATE claims SET worktree = ? WHERE worktree = ?`, key, p); err != nil {
			return false, err
		}
	}
	return false, nil
}

// stampedCheckoutPaths lists the distinct absolute paths still stamped on a
// lock or claim row. A key ("main", "worktrees/<name>") is never absolute.
func stampedCheckoutPaths(ctx context.Context, db sqlExecQuerier) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT worktree FROM locks WHERE worktree LIKE '/%' UNION SELECT worktree FROM claims WHERE worktree LIKE '/%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
