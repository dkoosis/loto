package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/store"
)

// ── `loto check --held` — the staged set must be MINE ─────────────────────
//
// `check --gate` asks "does a PEER hold any of these paths"; --held asks the
// question that gate cannot: "do *I* hold all of them". The difference is the
// whole bead (loto-7oik). mcp_agent_mail's pre-commit guard checks staged
// paths against other agents' exclusive reservations only, and the
// cc-plugins 2026-08-10 sweep passed exactly that check — it swept a peer's
// files while NOBODY held a reservation on them. A tree-wide `git commit -a`
// in a shared checkout is the same shape. Requiring the committer's own lock
// is the one addition that catches it.
//
// Advisory-first (survey D5): LOTO_GATE_MODE=warn renders the identical rows
// as ⚠ and exits 0, and every firing — warn or block — appends one
// store.EventStagedGateFired row, so "how often would this have refused a
// commit" is answerable from the events log before the default is
// reconsidered. The promotion is a follow-up bead filed at merge, never a
// timer in the code.

// heldArgs carries checkPreflight's parsed flags into this surface.
type heldArgs struct {
	gate, staged bool
	args         []string
}

// routeHeld validates the flag combination and dispatches. --held branches
// out of checkPreflight for the same reason --branch does: it asks a
// question the shared path machinery cannot answer, needing rename-aware
// staged loading (git diff --name-status -M, not --name-only). Combining it
// with --gate would be a caller who means one question and gets answered
// about the other, so it is refused.
func routeHeld(ctx context.Context, a heldArgs, stdout, stderr io.Writer) int {
	switch {
	case a.gate:
		fmt.Fprintln(stderr, "✗ --held and --gate ask opposite questions; pass one")
		return 2
	case !a.staged && len(a.args) == 0:
		fmt.Fprintln(stderr, "✗ --held needs --staged or at least one path")
		return 2
	default:
		return runCheckHeld(ctx, a.staged, a.args, stdout, stderr)
	}
}

// gateModeEnv names the advisory/blocking switch. Unset (or "block") blocks.
const gateModeEnv = "LOTO_GATE_MODE"

// gateModeWarn is the one value that downgrades a refusal to an advisory.
const gateModeWarn = "warn"

// held row states, in the vocabulary the report prints.
const (
	heldStateUnlocked  = "unlocked"   // nobody holds it, including me
	heldStatePeerLock  = "peer-lock"  // a live peer holds a lock/beacon on it
	heldStatePeerClaim = "peer-claim" // a live peer's claim covers it
)

// heldRow is one staged path this session may not commit, and why.
// RenamedFrom is the rename's other side when git reported this path as one
// half of an `R`/`C` entry — printed so a refusal on the destination still
// names the source (loto-7oik AC "both paths printed").
type heldRow struct {
	Path        string
	State       string
	HolderUUID  string
	Intent      string
	ExpiresAt   time.Time
	BlockerPath string // a claim's prefix; empty for the other states
	RenamedFrom string
}

// stagedPath is one entry of the staged change set: a path plus, for a
// rename or copy, the other side of it.
type stagedPath struct {
	Path        string
	RenamedFrom string
}

// loadStagedChangeSet reads the staged set with rename detection, NUL-safe.
//
// ‡ `--name-status -M -z`, not `--name-only`. Without -M a staged rename
// A→C reads as a delete plus an add and the two sides lose their
// relationship; with -M git emits `R<score>\0<old>\0<new>\0`, so BOTH sides
// enter the checked set and the destination row can name its source. -z is
// what makes it safe: without it git quotes and escapes paths containing
// spaces, quotes, or newlines, and a path with an embedded newline would
// split into two bogus entries — a path a committer controls deciding which
// paths the gate inspects.
//
// Every other status emits `<status>\0<path>\0`. The parser therefore reads
// a status token, then one path token, or two when the status starts with R
// or C. A truncated tail is dropped rather than guessed at.
func loadStagedChangeSet(ctx context.Context, repoTop string) ([]stagedPath, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	// cmd.Dir pinned to repoTop for the reason loadCheckTargets pins it
	// (loto-jff, gh#128): process cwd may be a different repo entirely.
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-status", "-M", "-z")
	if repoTop != "" {
		cmd.Dir = repoTop
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseStagedNameStatus(string(out)), nil
}

// parseStagedNameStatus turns `git diff --cached --name-status -M -z` output
// into the change set. Split out from the exec so the NUL framing — the half
// that has to be right — is testable without a git repo.
func parseStagedNameStatus(out string) []stagedPath {
	fields := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	var set []stagedPath
	for i := 0; i < len(fields); {
		status := fields[i]
		if status == "" {
			i++
			continue
		}
		want := 2
		if isRenameStatus(status) {
			want = 3
		}
		if i+want > len(fields) {
			break // truncated tail: drop it rather than guess a path
		}
		set = appendStagedEntry(set, fields[i+1:i+want])
		i += want
	}
	return set
}

// isRenameStatus reports whether a --name-status token carries two paths.
func isRenameStatus(status string) bool {
	return status[0] == 'R' || status[0] == 'C'
}

// appendStagedEntry adds one entry's paths: one for an ordinary status, both
// sides for a rename or copy, with the destination carrying its source.
func appendStagedEntry(set []stagedPath, paths []string) []stagedPath {
	if len(paths) == 1 {
		if paths[0] != "" {
			set = append(set, stagedPath{Path: paths[0]})
		}
		return set
	}
	oldPath, newPath := paths[0], paths[1]
	if oldPath != "" {
		set = append(set, stagedPath{Path: oldPath})
	}
	if newPath != "" {
		set = append(set, stagedPath{Path: newPath, RenamedFrom: oldPath})
	}
	return set
}

// decideHeld is the pure verdict: which of these paths may this session not
// commit. No IO, no clock beyond ec.Now — same shape as gateDecide, and
// tested the same way.
//
// ‡ "Held" means an EXCLUSIVE lock owned by me or my kin. A shared lock is a
// read declaration, and a beacon (a ModeShared record the tree-move guard
// mints) is not a write claim at all — counting either as held would let the
// hook-minted beacon that covers a whole session's tree satisfy the gate for
// every path in it, which is the gate switched off.
func decideHeld(entries []stagedPath, locks []domain.LockRecord, claims []domain.ClaimRecord, myUUID string, ec domain.EvalContext) []heldRow {
	var rows []heldRow
	seen := map[string]bool{}
	for _, e := range entries {
		t := domain.Target{Canonical: e.Path}
		if seen[e.Path] {
			continue
		}
		seen[e.Path] = true
		if heldByMe(t, locks, myUUID, ec) {
			continue
		}
		rows = append(rows, classifyUnheld(t, e.RenamedFrom, locks, claims, myUUID, ec))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		if rows[i].State != rows[j].State {
			return rows[i].State < rows[j].State
		}
		return rows[i].HolderUUID < rows[j].HolderUUID
	})
	return rows
}

// heldByMe reports whether this session (or its kin) holds a live exclusive
// lock on t.
func heldByMe(t domain.Target, locks []domain.LockRecord, myUUID string, ec domain.EvalContext) bool {
	for i := range locks {
		l := &locks[i]
		if !ec.SameTarget(t, l.Target) || l.EffectiveMode() != domain.ModeExclusive || l.IsBeacon() {
			continue
		}
		if string(l.OwnerUUID) != myUUID && !ec.IsKin(l.OwnerUUID) {
			continue
		}
		if ec.IsStale(*l) {
			continue // a lock `loto lock` would silently reclaim is not held
		}
		return true
	}
	return false
}

// classifyUnheld says WHY a path this session does not hold is unheld — a
// live peer's lock, a live peer's claim, or nobody at all. The order matters
// only for the fix line: a peer's path is `git restore --staged`, an
// unlocked one is `loto lock`.
//
// Liveness threshold is !IsStale, matching gateDecide rather than plain
// check's Classify==Alive: a hook-minted beacon carries the PID-0 sentinel,
// and under Classify it would read UNKNOWN and be reported as unlocked —
// telling the committer to `loto lock` a path a peer is sitting on.
func classifyUnheld(t domain.Target, renamedFrom string, locks []domain.LockRecord, claims []domain.ClaimRecord, myUUID string, ec domain.EvalContext) heldRow {
	row := heldRow{Path: t.Canonical, State: heldStateUnlocked, RenamedFrom: renamedFrom}
	for i := range locks {
		l := &locks[i]
		if !ec.SameTarget(t, l.Target) || string(l.OwnerUUID) == myUUID || ec.IsKin(l.OwnerUUID) || ec.IsStale(*l) {
			continue
		}
		if row.State == heldStatePeerLock && string(l.OwnerUUID) >= row.HolderUUID {
			continue // deterministic: the lowest holder uuid names the row
		}
		row.State, row.HolderUUID, row.Intent, row.ExpiresAt = heldStatePeerLock, string(l.OwnerUUID), l.Intent, l.ExpiresAt
		row.BlockerPath = ""
	}
	if row.State == heldStatePeerLock {
		return row
	}
	for i := range claims {
		c := &claims[i]
		if ec.IsKin(c.OwnerUUID) || !ec.ClaimCovers(*c, t.Canonical, myUUID) || ec.ClaimIsStale(*c) {
			continue
		}
		if row.State == heldStatePeerClaim && string(c.OwnerUUID) >= row.HolderUUID {
			continue
		}
		row.State, row.HolderUUID, row.Intent, row.ExpiresAt = heldStatePeerClaim, string(c.OwnerUUID), c.Intent, c.ExpiresAt
		row.BlockerPath = c.PathPrefix
	}
	return row
}

// heldGlyph is ✗ in blocking mode and ⚠ in warn mode — the ONLY difference
// between the two renderings, so "same output as ⚠ rows" is structural
// rather than a second formatter that can drift.
func heldGlyph(warn bool) string {
	if warn {
		return "⚠"
	}
	return "✗"
}

// printHeld renders the verdict. Deterministic: rows arrive sorted from
// decideHeld and the fix block's path lists are sorted, so the same store
// state produces byte-identical output.
func printHeld(stdout io.Writer, rows []heldRow, checked int, warn bool) {
	if len(rows) == 0 {
		fmt.Fprintf(stdout, "✓ held count=%d\n", checked)
		return
	}
	g := heldGlyph(warn)
	unlocked, peer := 0, 0
	for i := range rows {
		if rows[i].State == heldStateUnlocked {
			unlocked++
		} else {
			peer++
		}
	}
	fmt.Fprintf(stdout, "%s unheld count=%d unlocked=%d peer=%d staged=%d\n", g, len(rows), unlocked, peer, checked)
	for i := range rows {
		printHeldRow(stdout, g, &rows[i])
	}
	printHeldFix(stdout, rows)
}

func printHeldRow(stdout io.Writer, g string, r *heldRow) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s path=%s state=%s", g, relPath(r.Path), r.State)
	if r.RenamedFrom != "" {
		fmt.Fprintf(&b, " renamed_from=%s", relPath(r.RenamedFrom))
	}
	if r.State != heldStateUnlocked {
		fmt.Fprintf(&b, " blocker=%s", r.HolderUUID)
		if r.BlockerPath != "" {
			fmt.Fprintf(&b, " prefix=%s", relPath(r.BlockerPath))
		}
		fmt.Fprintf(&b, " intent=%q expires_at=%s", r.Intent, r.ExpiresAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintln(stdout, b.String())
}

// printHeldFix emits the one actionable block: `loto lock` for the paths
// nobody holds, `git restore --staged` for a peer's. Two lines at most, each
// carrying every path in its class — a per-row block would repeat the same
// command once per path for the common tree-wide-add case.
func printHeldFix(stdout io.Writer, rows []heldRow) {
	var mine, theirs []string
	for i := range rows {
		q := shellQuote(relPath(rows[i].Path))
		if rows[i].State == heldStateUnlocked {
			mine = append(mine, q)
		} else {
			theirs = append(theirs, q)
		}
	}
	sort.Strings(mine)
	sort.Strings(theirs)
	fmt.Fprintln(stdout, "```bash")
	if len(mine) > 0 {
		fmt.Fprintf(stdout, "loto lock %s -t \"<bead>: intent\"  # take what you are about to commit\n", strings.Join(mine, " "))
	}
	if len(theirs) > 0 {
		fmt.Fprintf(stdout, "git restore --staged %s  # a peer holds these; leave them out of your commit\n", strings.Join(theirs, " "))
	}
	fmt.Fprintln(stdout, "```")
}

// heldWarnMode reports whether LOTO_GATE_MODE downgrades a refusal to an
// advisory, and warns on a value it does not understand. An unrecognized
// value BLOCKS — a typo'd mode must not silently disarm the gate.
func heldWarnMode(stderr io.Writer) bool {
	switch v := strings.TrimSpace(os.Getenv(gateModeEnv)); v {
	case "", "block":
		return false
	case gateModeWarn:
		return true
	default:
		fmt.Fprintf(stderr, "⚠ %s=%q unrecognized gate=blocking\n", gateModeEnv, v)
		return false
	}
}

// recordHeldFiring appends the advisory-first firing counter: one row per
// firing, kind staged_lock_gate_fired, no target (a firing is a
// commit-scoped fact — same shape as gate_bypass). Best-effort: a store that
// cannot take the row does not change the verdict, and the loss is said out
// loud rather than swallowed.
func recordHeldFiring(rt *runtime, stderr io.Writer, rows []heldRow, warn bool) {
	unlocked, peer := 0, 0
	for i := range rows {
		if rows[i].State == heldStateUnlocked {
			unlocked++
		} else {
			peer++
		}
	}
	mode := "block"
	if warn {
		mode = gateModeWarn
	}
	detail, err := json.Marshal(struct {
		Mode     string `json:"mode"`
		Unheld   int    `json:"unheld"`
		Unlocked int    `json:"unlocked"`
		Peer     int    `json:"peer"`
	}{mode, len(rows), unlocked, peer})
	if err != nil {
		detail = nil
	}
	if _, err := rt.Store.AppendEvent(rt.Ctx, domain.Event{
		Kind:      store.EventStagedGateFired,
		ActorUUID: rt.Agent.UUID,
		Reason:    mode,
		Detail:    string(detail),
		CreatedAt: time.Now(),
	}); err != nil {
		fmt.Fprintf(stderr, "⚠ firing-counter=unrecorded err=%q\n", err)
	}
}

// runCheckHeld is the IO runner for `loto check --held [--staged] [<path>...]`.
//
// Fail-open matches `check --gate` exactly, and for the same reason: an
// unpinned identity mints a throwaway UUID that owns nothing, so every path
// would read as unheld and every commit in the repo would be refused. The
// notice goes to stderr — a hook that exits 0 after writing to stdout leaves
// the model blind to the fact the gate never ran (loto-tzmv.8).
func runCheckHeld(ctx context.Context, staged bool, posArgs []string, stdout, stderr io.Writer) int {
	warnIfContractStale(stderr)
	repoTop, _ := repoTopForCwd(ctx)

	entries, code := heldTargets(ctx, repoTop, staged, posArgs, stdout, stderr)
	if code != 0 || len(entries) == 0 {
		return code
	}

	if !identity.PinnedByEnv() {
		fmt.Fprintln(stderr, "⚠ identity=unpinned gate=fail-open")
		return 0
	}
	rt, err := openRuntime(ctx)
	if err != nil {
		return gateInfraUnreachable(stderr, err)
	}
	defer rt.Close()
	locks, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		return gateInfraUnreachable(stderr, err)
	}
	claims, err := rt.Store.ListClaims(rt.Ctx)
	if err != nil {
		return gateInfraUnreachable(stderr, err)
	}
	ec := domain.EvalContext{Now: time.Now(), Live: memoLiveProbe(rt.liveProbe()), CaseFold: rt.CaseFold}
	kin, err := parentKin(ctx)
	if err != nil {
		return gateInfraUnreachable(stderr, err)
	}
	ec.Kin = kin

	warn := heldWarnMode(stderr)
	rows := decideHeld(entries, locks, claims, rt.Agent.UUID, ec)
	printHeld(stdout, rows, len(entries), warn)
	if len(rows) == 0 {
		return 0
	}
	recordHeldFiring(rt, stderr, rows, warn)
	if warn {
		return 0
	}
	return 1
}

// heldTargets loads and canonicalizes the paths --held will judge. Returns
// an empty set (and 0) when there is nothing staged — printing the same
// `✓ no paths` plain check prints, because an empty commit is not a refusal.
func heldTargets(ctx context.Context, repoTop string, staged bool, posArgs []string, stdout, stderr io.Writer) ([]stagedPath, int) {
	var raw []stagedPath
	if staged {
		set, err := loadStagedChangeSet(ctx, repoTop)
		if err != nil {
			fmt.Fprintf(stderr, "✗ git diff: %v\n", err)
			return nil, 3
		}
		raw = set
	} else {
		for _, p := range posArgs {
			raw = append(raw, stagedPath{Path: p})
		}
	}
	if len(raw) == 0 {
		fmt.Fprintln(stdout, "✓ no paths")
		return nil, 0
	}

	// --staged paths come from git run with cmd.Dir=repoTop, so they are
	// repo-root-relative by construction; typed paths resolve against the
	// caller's cwd. Same provenance fork checkPreflight applies.
	base := callerBase()
	if staged {
		base = repoTop
	}
	out, invalid := resolveStagedPaths(base, repoTop, raw)
	if len(invalid) > 0 {
		sort.Slice(invalid, func(i, j int) bool { return invalid[i].Path < invalid[j].Path })
		printCheckInvalid(stdout, invalid)
		return nil, 2
	}
	return out, 0
}

// resolveStagedPaths canonicalizes both sides of every entry. A rename's
// source that fails to resolve — the common case, since the move already
// deleted it from disk — keeps its raw form rather than failing the entry:
// it is printed, never matched against a lock.
func resolveStagedPaths(base, repoTop string, raw []stagedPath) (out []stagedPath, invalid []checkInvalid) {
	cc := newCaseCache()
	for _, e := range raw {
		t, err := resolveCLITarget(cc, base, repoTop, e.Path)
		if err != nil {
			invalid = append(invalid, checkInvalid{Path: e.Path, Reason: classifyCanonicalizeErr(err)})
			continue
		}
		from := e.RenamedFrom
		if from != "" {
			if ft, ferr := resolveCLITarget(cc, base, repoTop, from); ferr == nil {
				from = ft.Canonical
			}
		}
		out = append(out, stagedPath{Path: t.Canonical, RenamedFrom: from})
	}
	return out, invalid
}
