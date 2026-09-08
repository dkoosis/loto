package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/render"
)

// ── `loto check --moved` — the tree moved under a peer's lock ─────────────
//
// A checkout or switch rewrites working-tree files with no warning to anyone
// holding them. git has no pre-checkout hook, so there is nothing to refuse
// with; post-checkout is the only native hook on this event and it fires
// after the damage. mcp_agent_mail has no tree-move guard at all.
//
// So this is advisory by construction, not by choice (loto-ea8y.2): the move
// is never blocked, and the agent that made it learns which peer-held paths
// it just rewrote. Exit is always 0 — a post-checkout hook's non-zero status
// stops the chain and would keep 50-beads from running, for a warning.

// movedRow is one peer-held path a tree move changed.
type movedRow struct {
	Path        string
	Kind        string
	HolderUUID  string
	Intent      string
	ExpiresAt   time.Time
	BlockerPath string
}

// loadMovedPaths lists the paths that differ between two HEADs, NUL-safe.
//
// ‡ `--no-renames`, and it is the flag that does the work. The question here
// is only "which working-tree files did this move rewrite", and BOTH sides of
// a rename were rewritten — the source was removed, the destination written.
// Omitting -M does not produce that: diff.renames has defaulted to true since
// git 2.9, so a rename reports the DESTINATION alone and a peer's lock on the
// source is never examined (measured on git 2.55.0; loto-l9ve). --no-renames
// reports the delete and the add separately, which is what the advisory owes
// its reader. `--held` keeps -M for the opposite reason: there a rename's two
// sides are one intent that both need locking, and the destination row has to
// be able to name its source.
func loadMovedPaths(ctx context.Context, repoTop, oldHead, newHead string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "--no-renames", "-z", oldHead, newHead)
	if repoTop != "" {
		cmd.Dir = repoTop
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var paths []string
	for p := range strings.SplitSeq(strings.TrimRight(string(out), "\x00"), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// collapseMovedRows turns gateDecide's per-blocker rows into ONE row per
// path — the shape the bead's Rule asks for ("one ⚠ row per path with the
// holder"). A lock beats a claim when both cover a path: the lock names the
// agent actually sitting in the file, which is who the mover has to go talk
// to. Within a kind the lowest holder uuid wins, so the choice is stable.
func collapseMovedRows(deny []render.GateDenyRow) []movedRow {
	best := map[string]render.GateDenyRow{}
	for i := range deny {
		d := deny[i]
		cur, seen := best[d.Path]
		if !seen || movedRowBetter(d, cur) {
			best[d.Path] = d
		}
	}
	rows := make([]movedRow, 0, len(best))
	for _, d := range best {
		rows = append(rows, movedRow{
			Path: d.Path, Kind: d.Kind, HolderUUID: d.HolderUUID,
			Intent: d.Intent, ExpiresAt: d.ExpiresAt, BlockerPath: d.BlockerPath,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return rows
}

func movedRowBetter(candidate, current render.GateDenyRow) bool {
	if candidate.Kind != current.Kind {
		return candidate.Kind == render.GateKindLock
	}
	return candidate.HolderUUID < current.HolderUUID
}

// printMoved renders the advisory. The empty case carries an explicit
// header (.claude/rules/design.md: silence looks like a crash) — the
// post-checkout entry is what turns that ✓ line into the silence git's
// output deserves, rather than the CLI guessing that its caller wants
// nothing.
func printMoved(stdout io.Writer, rows []movedRow) {
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "✓ moved-peer-locks count=0")
		return
	}
	fmt.Fprintf(stdout, "⚠ moved-peer-locks count=%d\n", len(rows))
	for i := range rows {
		r := &rows[i]
		var b strings.Builder
		fmt.Fprintf(&b, "⚠ path=%s kind=%s blocker=%s", relPath(r.Path), r.Kind, r.HolderUUID)
		if r.Kind == render.GateKindClaim && r.BlockerPath != "" {
			fmt.Fprintf(&b, " prefix=%s", relPath(r.BlockerPath))
		}
		fmt.Fprintf(&b, " intent=%q expires_at=%s", r.Intent, r.ExpiresAt.UTC().Format(time.RFC3339))
		fmt.Fprintln(stdout, b.String())
	}
	fmt.Fprintln(stdout, "ℹ options=tell-the-holder|move-back|carry-on — the move already happened")
}

// runCheckMoved is the IO runner for
// `loto check --moved <old-head> <new-head> [<is-branch-checkout>]`.
//
// The third operand is git's own post-checkout flag (1 = branch checkout,
// 0 = file checkout). It is accepted and ignored rather than refused, so the
// hook can forward "$@" verbatim: a file checkout passes the same HEAD
// twice, which diffs to nothing on its own.
//
// EVERY failure path exits 0 with a stderr notice. An advisory that turns a
// detached HEAD, a shallow clone, or an unreachable store into a non-zero
// post-checkout status would stop the hook chain — the reporting surface
// breaking the thing it reports on.
func runCheckMoved(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || len(args) > 3 {
		fmt.Fprintln(stderr, "✗ --moved wants <old-head> <new-head> [<is-branch-checkout>]")
		return 2
	}
	oldHead, newHead := args[0], args[1]
	if oldHead == newHead {
		printMoved(stdout, nil)
		return 0
	}
	repoTop, err := repoTopForCwd(ctx)
	if err != nil || repoTop == "" {
		fmt.Fprintln(stderr, "⚠ repo=unreadable moved=fail-open")
		return 0
	}
	paths, err := loadMovedPaths(ctx, repoTop, oldHead, newHead)
	if err != nil {
		// The null SHA git passes on a first checkout, a shallow clone, an
		// unborn branch: no diff to read, nothing to warn about.
		fmt.Fprintf(stderr, "⚠ diff=unreadable moved=fail-open err=%q\n", err)
		return 0
	}
	if len(paths) == 0 {
		printMoved(stdout, nil)
		return 0
	}
	rows, ok := movedPeerRows(ctx, repoTop, paths, stderr)
	if !ok {
		return 0 // the reason is already on stderr; the move is never blocked
	}
	printMoved(stdout, rows)
	return 0
}

// movedPeerRows reads the store and returns one row per peer-held path.
// ok=false means the advisory could not be computed — every such case has
// already said why on stderr, and the caller exits 0 regardless.
func movedPeerRows(ctx context.Context, repoTop string, paths []string, stderr io.Writer) ([]movedRow, bool) {
	if !identity.PinnedByEnv() {
		fmt.Fprintln(stderr, "⚠ identity=unpinned moved=fail-open")
		return nil, false
	}
	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ store=unreachable moved=fail-open err=%q\n", err)
		return nil, false
	}
	defer rt.Close()
	locks, lerr := rt.Store.ListLocks(rt.Ctx)
	claims, cerr := rt.Store.ListClaims(rt.Ctx)
	if lerr != nil || cerr != nil {
		fmt.Fprintln(stderr, "⚠ store=unreadable moved=fail-open")
		return nil, false
	}
	// git printed these, so they are repo-root-relative already.
	targets, invalid := resolveCheckTargets(repoTop, repoTop, paths)
	if len(invalid) > 0 {
		// A path the move deleted no longer resolves. That is data, not an
		// error: report what did resolve rather than refusing the advisory.
		fmt.Fprintf(stderr, "⚠ unresolved=%d moved=partial\n", len(invalid))
	}
	kin, err := parentKin(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ kin=unresolved moved=fail-open err=%q\n", err)
		return nil, false
	}
	ec := domain.EvalContext{Now: time.Now(), Live: memoLiveProbe(rt.liveProbe()), CaseFold: rt.CaseFold, Kin: kin}
	return collapseMovedRows(gateDecide(targets, locks, claims, rt.Agent.UUID, ec)), true
}
