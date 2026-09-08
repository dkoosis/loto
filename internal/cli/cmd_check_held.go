package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
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
// ‡ It SHIPS ADVISORY (survey D5, epic loto-ea8y "New gates ship in warn mode
// and promote to blocking on evidence"): with LOTO_GATE_MODE unset the rows
// render as ⚠ and the exit is 0, so a commit is never refused by a default.
// LOTO_GATE_MODE=block is what turns the same rows into ✗ and exit 1.
//
// The rows are identical either way — only the glyph and the exit differ — so
// what the advisory period measures is exactly what the blocking period will
// refuse. Every firing, warn or block, appends one
// store.EventStagedGateFired row, so "how often would this have refused a
// commit" is answerable from the events log. Promoting the default is a
// follow-up bead read off that counter, never a timer in the code.

// heldMovedArgs carries checkPreflight's parsed flags into the two surfaces
// that branch out of it.
//
// cwdUnknown rides along because --held takes PATHS: dropping the flag on this
// route let a relative token resolve against loto's own cwd and report a
// same-named file in another directory as held (loto-l9ve). --moved's operands
// are two HEADs, so the flag has nothing to bind to there.
type heldMovedArgs struct {
	held, moved, gate, staged, cwdUnknown bool
	args                                  []string
}

// routeHeldOrMoved validates the flag combination and dispatches. --held and
// --moved branch out of checkPreflight for the same reason --branch does:
// each asks a question the shared path machinery cannot answer. --held needs
// rename-aware staged loading (git diff --name-status -M, not --name-only),
// and --moved's operands are two HEADs rather than paths. Combining either
// with --gate would be a caller who means one question and gets answered
// about the other, so it is refused.
func routeHeldOrMoved(ctx context.Context, a heldMovedArgs, stdout, stderr io.Writer) int {
	switch {
	case a.held && a.moved:
		fmt.Fprintln(stderr, "✗ --held and --moved ask different questions; pass one")
		return 2
	case a.held && a.gate:
		fmt.Fprintln(stderr, "✗ --held and --gate ask opposite questions; pass one")
		return 2
	case a.held && !a.staged && len(a.args) == 0:
		fmt.Fprintln(stderr, "✗ --held needs --staged or at least one path")
		return 2
	case a.held:
		return runCheckHeld(ctx, heldRunArgs{staged: a.staged, cwdUnknown: a.cwdUnknown, args: a.args}, stdout, stderr)
	case a.gate || a.staged:
		fmt.Fprintln(stderr, "✗ --moved takes two HEADs, no other check flag")
		return 2
	default:
		return runCheckMoved(ctx, a.args, stdout, stderr)
	}
}

// gateModeEnv names the advisory/blocking switch. Unset means warn.
const gateModeEnv = "LOTO_GATE_MODE"

// The two modes. Unset resolves to gateModeWarn: a gate ships advisory and
// is promoted on counted evidence, so the blocking mode is the one an
// operator has to ask for by name.
const (
	gateModeWarn  = "warn"
	gateModeBlock = "block"
)

// held row states, in the vocabulary the report prints.
const (
	heldStateUnlocked     = "unlocked"     // nobody holds it, including me
	heldStatePeerLock     = "peer-lock"    // a live peer holds a lock/beacon on it
	heldStatePeerClaim    = "peer-claim"   // a live peer's claim covers it
	heldStateUnresolvable = "unresolvable" // the gate could not canonicalize it
	heldStateUnlockable   = "unlockable"   // no session could ever hold it
)

// heldRow is one staged path this session may not commit, and why.
// RenamedFrom is the rename's other side when git reported this path as one
// half of an `R`/`C` entry — printed so a refusal on the destination still
// names the source (loto-7oik AC "both paths printed").
//
// Reason carries the design.md token for the two states that have one:
// `unresolvable` (classifyCanonicalizeErr's token) and `unlockable`
// (statFileTargetReason's — the SAME token `loto lock` would print when it
// refused the path, so the exemption and the refusal cannot drift apart).
type heldRow struct {
	Path        string
	State       string
	Reason      string
	HolderUUID  string
	Intent      string
	ExpiresAt   time.Time
	BlockerPath string // a claim's prefix; empty for the other states
	RenamedFrom string
}

// stagedPath is one entry of the staged change set: a path plus, for a
// rename or copy, the other side of it.
//
// Unlockable is non-empty when `loto lock` would REFUSE this path — a symlink
// or a non-regular target such as a submodule's gitlink directory. Such a path
// can never be held by any session, so demanding a lock on it would print a
// remedy that cannot succeed (loto-pgio). The value is the reason token.
type stagedPath struct {
	Path        string
	RenamedFrom string
	Unlockable  string
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
// It returns two slices. rows are the verdict — what the gate would refuse in
// block mode. notes are ℹ rows: paths the gate looked at and is telling the
// reader it does NOT protect, which never affect the exit code and never fire
// the counter.
func decideHeld(entries []stagedPath, unresolved []checkInvalid, locks []domain.LockRecord, claims []domain.ClaimRecord, myUUID string, ec domain.EvalContext) (rows, notes []heldRow) {
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
		row := classifyUnheld(t, e.RenamedFrom, locks, claims, myUUID, ec)
		// ‡ The exemption applies ONLY to the unlocked verdict. A peer's lock
		// on a path that is a symlink today (it was a regular file when they
		// took it) is still real, still actionable, and still named — the
		// remedy there is `git restore --staged`, which always works.
		if e.Unlockable != "" && row.State == heldStateUnlocked {
			notes = append(notes, heldRow{Path: e.Path, State: heldStateUnlockable, Reason: e.Unlockable, RenamedFrom: e.RenamedFrom})
			continue
		}
		rows = append(rows, row)
	}
	// A path the gate could not canonicalize is a check it could not complete,
	// so it is a verdict row and not a note: `git restore --staged` takes it
	// out of the commit, which is a remedy that works.
	for _, iv := range unresolved {
		if seen[iv.Path] {
			continue
		}
		seen[iv.Path] = true
		rows = append(rows, heldRow{Path: iv.Path, State: heldStateUnresolvable, Reason: iv.Reason})
	}
	sortHeldRows(rows)
	sortHeldRows(notes)
	return rows, notes
}

func sortHeldRows(rows []heldRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		if rows[i].State != rows[j].State {
			return rows[i].State < rows[j].State
		}
		return rows[i].HolderUUID < rows[j].HolderUUID
	})
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

// rowPath renders a path for a one-row-per-line surface. A control character
// in a NAME would otherwise split the row or move the cursor, and since
// loto-pgio the gate accepts such a name rather than refusing the whole staged
// batch over it (domain.ProvenanceGit) — so the escape has to happen here, at
// the point of print. An ordinary path is untouched, so every existing golden
// is byte-identical.
func rowPath(p string) string {
	rel := relPath(p)
	if domain.HasControl(rel) {
		return strconv.Quote(rel)
	}
	return rel
}

// printHeld renders the verdict. Deterministic: rows arrive sorted from
// decideHeld and the fix block's path lists are sorted, so the same store
// state produces byte-identical output.
func printHeld(stdout io.Writer, rows, notes []heldRow, checked int, warn bool) {
	if len(rows) == 0 {
		fmt.Fprintf(stdout, "✓ held count=%d\n", checked)
		printHeldNotes(stdout, notes)
		return
	}
	g := heldGlyph(warn)
	unlocked, peer, unresolvable := 0, 0, 0
	for i := range rows {
		switch rows[i].State {
		case heldStateUnlocked:
			unlocked++
		case heldStateUnresolvable:
			unresolvable++
		default:
			peer++
		}
	}
	fmt.Fprintf(stdout, "%s unheld count=%d unlocked=%d peer=%d staged=%d", g, len(rows), unlocked, peer, checked)
	// Appended only when it happened: an unresolvable staged path is rare, and
	// the field would otherwise widen every header line for nothing.
	if unresolvable > 0 {
		fmt.Fprintf(stdout, " unresolvable=%d", unresolvable)
	}
	fmt.Fprintln(stdout)
	for i := range rows {
		printHeldRow(stdout, g, &rows[i])
	}
	printHeldNotes(stdout, notes)
	printHeldFix(stdout, rows)
}

// printHeldNotes emits the ℹ rows: staged paths the gate is NOT protecting,
// each naming the reason `loto lock` would refuse it. Saying so out loud is
// the whole point — a gate that silently skips a path teaches the reader it
// covered one.
func printHeldNotes(stdout io.Writer, notes []heldRow) {
	for i := range notes {
		r := &notes[i]
		var b strings.Builder
		fmt.Fprintf(&b, "ℹ path=%s state=%s reason=%s", rowPath(r.Path), r.State, r.Reason)
		if r.RenamedFrom != "" {
			fmt.Fprintf(&b, " renamed_from=%s", rowPath(r.RenamedFrom))
		}
		fmt.Fprint(&b, " gate=not-protected")
		fmt.Fprintln(stdout, b.String())
	}
}

func printHeldRow(stdout io.Writer, g string, r *heldRow) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s path=%s state=%s", g, rowPath(r.Path), r.State)
	if r.Reason != "" {
		fmt.Fprintf(&b, " reason=%s", r.Reason)
	}
	if r.RenamedFrom != "" {
		fmt.Fprintf(&b, " renamed_from=%s", rowPath(r.RenamedFrom))
	}
	if r.State != heldStateUnlocked && r.State != heldStateUnresolvable {
		fmt.Fprintf(&b, " blocker=%s", r.HolderUUID)
		if r.BlockerPath != "" {
			fmt.Fprintf(&b, " prefix=%s", relPath(r.BlockerPath))
		}
		fmt.Fprintf(&b, " intent=%q expires_at=%s", r.Intent, r.ExpiresAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintln(stdout, b.String())
}

// heldFixLine is one class of remedy: the paths it applies to, and the command
// that clears them. Every line here is a command that would ACTUALLY satisfy
// the gate — a path no session could lock never reaches this block (loto-pgio).
type heldFixLine struct {
	format string
	paths  []string
}

// printHeldFix emits the one actionable block: `loto lock` for the paths
// nobody holds, `git restore --staged` for a peer's, and the same unstage for
// a path the gate could not read. Three lines at most, each carrying every
// path in its class — a per-row block would repeat the same command once per
// path for the common tree-wide-add case.
//
// ‡ A path whose NAME carries a control character is omitted, with an ℹ row
// saying so. Every shell spelling of such a token is either non-portable
// ($'...' is bash/zsh, not sh) or a literal newline inside the fenced block
// that a reader would copy wrong — and a remedy printed wrong is the defect
// this bead exists to remove.
func printHeldFix(stdout io.Writer, rows []heldRow) {
	mine := heldFixLine{format: "loto lock %s -t \"<bead>: intent\"  # take what you are about to commit\n"}
	theirs := heldFixLine{format: "git restore --staged %s  # a peer holds these; leave them out of your commit\n"}
	unreadable := heldFixLine{format: "git restore --staged %s  # loto cannot read these paths; leave them out of your commit\n"}
	omitted := 0
	for i := range rows {
		rel := relPath(rows[i].Path)
		if domain.HasControl(rel) {
			omitted++
			continue
		}
		q := shellQuote(rel)
		switch rows[i].State {
		case heldStateUnlocked:
			mine.paths = append(mine.paths, q)
		case heldStateUnresolvable:
			unreadable.paths = append(unreadable.paths, q)
		default:
			theirs.paths = append(theirs.paths, q)
		}
	}
	if omitted > 0 {
		fmt.Fprintf(stdout, "ℹ fix-omitted count=%d reason=control-character-in-name\n", omitted)
	}
	lines := []*heldFixLine{&mine, &theirs, &unreadable}
	if len(mine.paths)+len(theirs.paths)+len(unreadable.paths) == 0 {
		return // every path was omitted: an empty fence is not a fix block
	}
	fmt.Fprintln(stdout, "```bash")
	for _, line := range lines {
		if len(line.paths) == 0 {
			continue
		}
		sort.Strings(line.paths)
		fmt.Fprintf(stdout, line.format, strings.Join(line.paths, " "))
	}
	fmt.Fprintln(stdout, "```")
}

// heldWarnMode resolves LOTO_GATE_MODE. Unset is warn — the advisory-first
// default the epic's Rules and its own acceptance criteria call for
// ("session staging an unlocked file gets a ⚠ warn row and the commit
// proceeds"). Only LOTO_GATE_MODE=block refuses.
//
// ‡ An unrecognized value falls back to the DEFAULT and says so, rather than
// to blocking. Blocking is the mode an operator opts into by name; a value
// nobody meant must not be able to start refusing commits, and the ⚠ names
// the string so a typo is visible rather than silently obeyed.
func heldWarnMode(stderr io.Writer) bool {
	switch v := strings.TrimSpace(os.Getenv(gateModeEnv)); v {
	case "", gateModeWarn:
		return true
	case gateModeBlock:
		return false
	default:
		fmt.Fprintf(stderr, "⚠ %s=%q unrecognized gate=%s\n", gateModeEnv, v, gateModeWarn)
		return true
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
	mode := gateModeBlock
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
	// ‡ AppendEventRotating, not AppendEvent (loto-241n). This counter is the
	// one event a session appends on every commit while acquiring no lock at
	// all — the exact workload the advisory rollout is meant to measure — and
	// the plain append never rotates, so the events table would grow past both
	// retention bounds with nothing trimming it.
	if _, err := rt.Store.AppendEventRotating(rt.Ctx, domain.Event{
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
func runCheckHeld(ctx context.Context, a heldRunArgs, stdout, stderr io.Writer) int {
	warnIfContractStale(stderr)
	repoTop, _ := repoTopForCwd(ctx)

	// loto-l9ve: --cwd-unknown binds here exactly as it does on the ordinary
	// check route (cmd_check.go), and for the same reason — a relative token
	// with no knowable base would silently resolve against loto's own cwd and
	// report a same-named file in another directory as held. Scoped to typed
	// paths: --staged tokens come from git run with cmd.Dir=repoTop, so they
	// are repo-root-relative by construction and stay allowed.
	if a.cwdUnknown && !a.staged {
		if rc, refused := refuseUnresolvableRelative(stdout, a.args); refused {
			return rc
		}
	}

	entries, unresolved, code := heldTargets(ctx, repoTop, a.staged, a.args, stdout, stderr)
	if code != 0 || (len(entries) == 0 && len(unresolved) == 0) {
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
	rows, notes := decideHeld(entries, unresolved, locks, claims, rt.Agent.UUID, ec)
	printHeld(stdout, rows, notes, len(entries)+len(unresolved), warn)
	if len(rows) == 0 {
		return 0
	}
	recordHeldFiring(rt, stderr, rows, warn)
	if warn {
		return 0
	}
	return 1
}

// heldRunArgs is runCheckHeld's parsed input.
type heldRunArgs struct {
	staged, cwdUnknown bool
	args               []string
}

// heldTargets loads and canonicalizes the paths --held will judge. Returns
// an empty set (and 0) when there is nothing staged — printing the same
// `✓ no paths` plain check prints, because an empty commit is not a refusal.
//
// ‡ A STAGED path that will not canonicalize is returned as an unresolved row,
// never as an exit-2 refusal of the whole batch. That refusal was a full
// bypass: the pre-commit leg proceeds on any exit but 1, so one staged file
// with an odd name switched the ownership check off for every other path in
// the commit (loto-pgio). A TYPED path is still exit 2 — that is a caller
// error, with a caller to correct it.
func heldTargets(ctx context.Context, repoTop string, staged bool, posArgs []string, stdout, stderr io.Writer) (entries []stagedPath, unresolved []checkInvalid, code int) {
	var raw []stagedPath
	if staged {
		set, err := loadStagedChangeSet(ctx, repoTop)
		if err != nil {
			fmt.Fprintf(stderr, "✗ git diff: %v\n", err)
			return nil, nil, 3
		}
		raw = set
	} else {
		for _, p := range posArgs {
			raw = append(raw, stagedPath{Path: p})
		}
	}
	if len(raw) == 0 {
		fmt.Fprintln(stdout, "✓ no paths")
		return nil, nil, 0
	}

	// --staged paths come from git run with cmd.Dir=repoTop, so they are
	// repo-root-relative by construction; typed paths resolve against the
	// caller's cwd. Same provenance fork checkPreflight applies.
	base := callerBase()
	if staged {
		base = repoTop
	}
	out, invalid := resolveStagedPaths(base, repoTop, raw, staged)
	sort.Slice(invalid, func(i, j int) bool { return invalid[i].Path < invalid[j].Path })
	if len(invalid) > 0 && !staged {
		printCheckInvalid(stdout, invalid)
		return nil, nil, 2
	}
	return out, invalid, 0
}

// resolveStagedPaths canonicalizes both sides of every entry and records, for
// each, whether `loto lock` could ever take it. A rename's source that fails
// to resolve — the common case, since the move already deleted it from disk —
// keeps its raw form rather than failing the entry: it is printed, never
// matched against a lock.
//
// fromGit selects the canonicalization policy. git-produced tokens are
// admitted with a `"` or a control character in the name (domain.ProvenanceGit):
// no shell ever touched them, so the unexpanded-token rule that refuses them
// is answering a question nobody asked.
func resolveStagedPaths(base, repoTop string, raw []stagedPath, fromGit bool) (out []stagedPath, invalid []checkInvalid) {
	resolve := resolveCLITarget
	if fromGit {
		resolve = resolveGitTarget
	}
	cc := newCaseCache()
	for _, e := range raw {
		t, err := resolve(cc, base, repoTop, e.Path)
		if err != nil {
			invalid = append(invalid, checkInvalid{Path: e.Path, Reason: classifyCanonicalizeErr(err)})
			continue
		}
		from := e.RenamedFrom
		if from != "" {
			if ft, ferr := resolve(cc, base, repoTop, from); ferr == nil {
				from = ft.Canonical
			}
		}
		out = append(out, stagedPath{Path: t.Canonical, RenamedFrom: from, Unlockable: unlockableReason(repoTop, t.Canonical)})
	}
	return out, invalid
}

// unlockableReason names why no session could ever hold a lock on this path,
// or "" when one could. It asks `loto lock`'s OWN validator
// (statFileTargetReason), so the gate's exemption is the lock verb's refusal
// by construction rather than a second list that can drift from it.
//
// Only the two permanent refusals count. `not-found` — a staged deletion, a
// rename's source — is NOT one of them: that path was lockable right up until
// the session deleted it, so the gate has a real thing to say about it.
func unlockableReason(repoTop, canonical string) string {
	switch reason := statFileTargetReason(repoTop, canonical, false); reason {
	case "symlink", reasonNotRegularFile:
		return reason
	default:
		return ""
	}
}
