package cli

import (
	"context"
	"fmt"
	"io"

	"loto/internal/identity"
)

// ── the no-stash red-first route (loto-f8xt) ──────────────────────────────
//
// Agents prove a test fails without their change with `git stash; <test>;
// git stash pop`, or `git checkout -- <file>` and an edit back. In a checkout
// another live session shares, both sweep that peer's uncommitted files too
// (sdlc 2026-09-26: 51 tree events from one stash/test/pop).
//
// Which hook sees which move, measured on git 2.55.0:
//   - git stash / stash pop run NO post-checkout. reference-transaction sees
//     every one as an update to refs/stash, and the I1 ref guard already
//     refuses it under two live sessions (cmd_hook_ref.go). That refusal
//     carries the route.
//   - git checkout -- <path>, git checkout HEAD -- <path> and git restore run
//     post-checkout with the same HEAD as $1 and $2, flag 0. The file is
//     already overwritten, so the route rides an advisory
//     (runFileCheckoutAdvisory) for next time.
//   - git reset --hard runs no post-checkout, and its ref rows (HEAD old ==
//     new) cannot be told from a commit's. It stays uncovered.
//
// The route is a throwaway worktree at HEAD: the new test is copied in, the
// fix is not, and the test runs there. Nothing in the shared tree is written.

// redFirstInfo names the route on the ℹ row above its fix block.
const redFirstInfo = "ℹ red-first=throwaway-worktree — run the test against HEAD plus only the new test; the shared tree is not touched"

// redFirstRoute is the route, one shell line per element, printed verbatim
// inside a ```bash block. The two <placeholders> are the only edits a reader
// makes; cmd/loto/testdata/script/guard_red_first_route.txtar runs the
// printed lines with them filled in. The test line is a subshell, so a red
// test (the point of the exercise) still reaches the remove line.
var redFirstRoute = []string{
	`red=$(mktemp -d) && git worktree add -q --detach "$red" HEAD`,
	`cp <new-test-file> "$red/<new-test-file>"`,
	`(cd "$red" && <test-command>)`,
	`git worktree remove -f "$red"`,
}

// fileCheckoutFlag is git's post-checkout $3 for a file checkout.
const fileCheckoutFlag = "0"

// runFileCheckoutAdvisory handles a post-checkout with the same HEAD on both
// sides and flag 0: a checkout -- or a restore. git does not name the paths
// it overwrote, so no per-path holder check is possible. Under two or more
// live sessions in this checkout it prints the advisory and the red-first
// route; otherwise the ✓ empty header.
//
// Gated like the I1 ref guard: no session id → not a session, nothing to
// say; fewer than two live sessions → no peer to hurt. `git restore
// --staged` arrives in the same shape and gets the advisory too; git hands
// the hook nothing to tell it apart.
//
// Exit is always 0 (the post-checkout contract, see runCheckMoved).
func runFileCheckoutAdvisory(ctx context.Context, stdout, stderr io.Writer) int {
	if identity.SessionIDFromEnv() == "" {
		printMoved(stdout, nil)
		return 0
	}
	repoTop, err := repoTopForCwd(ctx)
	if err != nil || repoTop == "" {
		fmt.Fprintln(stderr, "⚠ repo=unreadable moved=fail-open")
		return 0
	}
	live := identity.LiveOwnerUUIDsInRepo(repoTop)
	if len(live) < 2 {
		printMoved(stdout, nil)
		return 0
	}
	fmt.Fprintf(stdout, "⚠ shared-tree-file-checkout live=%d\n", len(live))
	fmt.Fprintln(stdout, "ℹ a checkout -- or restore overwrote working-tree files a live peer may be editing; git does not name them")
	fmt.Fprintln(stdout, redFirstInfo)
	fmt.Fprintln(stdout, "```bash")
	for _, line := range redFirstRoute {
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintln(stdout, "```")
	return 0
}
