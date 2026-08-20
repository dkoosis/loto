package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/render"
)

func init() { register("check", cmdCheck) } //nolint:gochecknoinits // command registry pattern

type checkConflict struct {
	Path     string
	Blocker  domain.LockRecord
	Blocking bool // true: provably-live exclusive holder → hard block (exit 1).
	// false: indeterminate/expiring liveness → advisory warn (does not set exit 1).
}

// checkPreflight parses check's flags, resolves the repo top, loads the target
// paths, and applies the pre-resolution refusals that must fire before any
// store IO. done=true means the caller must return rc immediately — the
// preflight already emitted whatever the user sees.
func checkPreflight(ctx context.Context, args []string, stdout, stderr io.Writer) (paths []string, base string, repoTop string, gate bool, rc int, done bool) {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	staged := fs.Bool("staged", false, "read paths from git diff --cached")
	gateFlag := fs.Bool("gate", false, "read-only deny gate: exit 1 if a foreign live claim or lock/beacon covers any path; never acquires, refreshes, or writes")
	cwdUnknown := fs.Bool("cwd-unknown", false, "the caller's working directory is not knowable here (e.g. mcp__trixi__agent_shell): refuse relative paths instead of resolving them against the wrong base")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return nil, "", "", false, 2, true
	}

	// Resolve repoTop before shelling out to git so `git diff --cached` runs
	// with cmd.Dir = repoTop (loto-jff, gh#128). Without that pin, the staged
	// diff inherits process cwd and reads the wrong repo's index in worktree /
	// nested-launch / scripted-invocation scenarios.
	repoTop, _ = repoTopForCwd(ctx)

	// The provenance fork, in one place: tokens the CALLER typed resolve against
	// the caller's cwd, tokens git produced (cmd.Dir=repoTop) are already
	// repo-root-relative (loto-3tv3 D3). Both check surfaces are fed from here,
	// so they cannot drift.
	base = callerBase()
	if *staged {
		base = repoTop
	}

	paths, code := loadCheckTargets(ctx, repoTop, *staged, fs.Args(), stderr)
	if code != 0 {
		return nil, base, repoTop, *gateFlag, code, true
	}
	if len(paths) == 0 {
		fmt.Fprintln(stdout, "✓ no paths")
		return nil, base, repoTop, *gateFlag, 0, true
	}

	// loto-tzmv.10: refuse before resolving, never guess. Scoped to paths the
	// CALLER typed — --staged paths come from `git diff --cached` run with
	// cmd.Dir = repoTop, so their base is repo root by construction and known
	// regardless of the caller's cwd. Refusing those would make
	// `--cwd-unknown --staged` reject every nonempty commit (Codex #247).
	// See refuseUnresolvableRelative.
	if *cwdUnknown && !*staged {
		if rc, refused := refuseUnresolvableRelative(stdout, paths); refused {
			return nil, base, repoTop, *gateFlag, rc, true
		}
	}
	return paths, base, repoTop, *gateFlag, 0, false
}

func cmdCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	paths, base, repoTop, gate, rc, done := checkPreflight(ctx, args, stdout, stderr)
	if done {
		return rc
	}

	// --gate is a distinct read-only decision surface (loto-vr2,
	// gate-design.md component 4): it consults claims (plain check never
	// does), denies on any foreign live lock/beacon regardless of mode, and
	// fails open on identity/store infra BEFORE opening the store. Branching
	// here — after path loading, before openRuntime — keeps every line below
	// this point (the plain-check path) untouched, so plain `loto check`
	// stays byte-identical (hard rule, plan "Plain loto check ... behavior
	// must stay byte-identical").
	if gate {
		return runCheckGate(ctx, paths, base, repoTop, stdout, stderr)
	}

	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	all, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	// Filter stale/dead-PID holders the same way AcquireLocks does
	// (reclaimStaleAndCollectBlockers → domain.IsStale): a lock that `loto lock`
	// would silently reclaim must not read as a hard conflict here (loto-9t0q).
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe()}

	// Resolve targets once, up front. Invalid input exits 2 before any claim
	// read — so the wasted ListClaims/resolve on the error path is gone, and
	// both the conflict scan and the foreign-claim advisory consume the same
	// resolved targets (no double resolution). Mirrors runCheckGate's
	// resolve-then-branch shape (gemini/coderabbit, #214).
	targets, invalid := resolveCheckTargets(base, repoTop, paths)
	if len(invalid) > 0 {
		printCheckInvalid(stdout, invalid)
		return 2
	}

	// Foreign-claim advisory (loto-qoq): computed up front so it emits on every
	// non-error plain-check exit — including the common no-conflict early
	// return below, which never reaches the bottom of this function.
	claimAdvisories := foreignClaimAdvisoriesFor(rt, targets, ec.Now)
	// Territory tags covering the REQUESTED targets (loto-z3y1). Computed here
	// for the same reason the claim advisory is: the no-conflict early return
	// below never reaches the bottom of this function, and a note that only
	// surfaced on the conflict path would miss every quiet check — which is
	// most of them. Plain check only: `check --gate` stays byte-identical.
	territoryNotes := territoryTagsForTargets(liveTerritoryTags(rt, ec.Now), targets, ec.Now)
	emitAfter := func(code int) int {
		emitForeignClaimAdvisories(stdout, claimAdvisories)
		render.EmitTerritoryTagRows(stdout, territoryNotes, "territory-tags", "")
		return code
	}

	rows := computeCheckConflicts(targets, all, rt.Agent.UUID, ec)
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "✓ no conflicts")
		return emitAfter(0)
	}
	if printCheckConflicts(stdout, rows) {
		return emitAfter(1)
	}
	return emitAfter(0)
}

type checkInvalid struct {
	Path   string
	Reason string
}

func printCheckInvalid(stdout io.Writer, rows []checkInvalid) {
	fmt.Fprintf(stdout, "✗ invalid count=%d\n", len(rows))
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(stdout, "✗ path=%s reason=%s\n", r.Path, r.Reason)
	}
}

func loadCheckTargets(ctx context.Context, repoTop string, staged bool, posArgs []string, stderr io.Writer) ([]string, int) {
	if !staged {
		return posArgs, 0
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	// Pin cmd.Dir = repoTop so the staged diff comes from the loto-resolved
	// repo, not from process cwd (loto-jff, gh#128).
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only", "-z")
	if repoTop != "" {
		cmd.Dir = repoTop
	}
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(stderr, "✗ git diff: %v\n", err)
		return nil, 3
	}
	var paths []string
	for p := range strings.SplitSeq(strings.TrimRight(string(out), "\x00"), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, 0
}

// resolveCheckTargets resolves raw CLI paths into targets plus invalid rows
// (sorted by original path, the deterministic-output rule) — the shared front
// half of plain check and check --gate, so invalid-target classification
// can't drift between the two codepaths (review nit, #211).
func resolveCheckTargets(base, repoTop string, paths []string) ([]domain.Target, []checkInvalid) {
	var targets []domain.Target
	var invalid []checkInvalid
	for _, raw := range paths {
		t, err := resolveCLITarget(base, repoTop, raw)
		if err != nil {
			invalid = append(invalid, checkInvalid{Path: raw, Reason: classifyCanonicalizeErr(err)})
			continue
		}
		targets = append(targets, t)
	}
	sort.Slice(invalid, func(i, j int) bool { return invalid[i].Path < invalid[j].Path })
	return targets, invalid
}

func computeCheckConflicts(targets []domain.Target, all []domain.LockRecord, myUUID string, ec domain.EvalContext) []checkConflict {
	var rows []checkConflict
	seen := map[string]bool{}
	for _, t := range targets {
		rows = appendCheckConflictsForTarget(rows, seen, t, all, myUUID, ec)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		return rows[i].Blocker.Target.Canonical < rows[j].Blocker.Target.Canonical
	})
	return rows
}

func appendCheckConflictsForTarget(rows []checkConflict, seen map[string]bool, t domain.Target, all []domain.LockRecord, myUUID string, ec domain.EvalContext) []checkConflict {
	// Probe as SHARED, not exclusive: the committer's question is "does any peer
	// hold a lease that excludes me?" Conflicts(shared, shared)=false (readers
	// coexist) and Conflicts(shared, exclusive)=true — so only an exclusive peer
	// conflicts, which is the intended check semantics (loto-k5el.2 T8). A stale
	// holder is filtered inside Conflicts (AcquireLocks would reclaim it; the
	// gate must not demand `unlock --force` for a reclaimable lock, loto-9t0q).
	probe := domain.LockRecord{Target: t, OwnerUUID: domain.AgentUUID(myUUID), Mode: domain.ModeShared}
	for i := range all {
		l := &all[i]
		if !ec.Conflicts(probe, *l) {
			continue
		}
		key := t.Canonical + "|" + l.Target.Canonical + "|" + string(l.OwnerUUID)
		if seen[key] {
			continue
		}
		seen[key] = true
		// Liveness gate (binding correction 4 / §check --staged): a provably-live
		// exclusive holder hard-blocks (exit 1); an indeterminate/expiring one
		// (PID-0 sentinel, cross-host) is an advisory warn that does not block.
		// Classify is .1's display-tier verdict (loto-k5el.1).
		blocking := ec.Classify(*l) == domain.LivenessAlive
		rows = append(rows, checkConflict{Path: t.Canonical, Blocker: all[i], Blocking: blocking})
	}
	return rows
}

// printCheckConflicts renders one row per conflict and returns whether any row
// is a hard blocker (provably-live exclusive holder). Blocking rows lead with ✗
// and carry an actionable force-unlock fix block; advisory rows lead with ✓
// (the committer may proceed) and a liveness=unknown field — they do not set
// exit 1 (loto-k5el.2 T8). The closed glyph vocabulary (design.md) is ✓/✗ only,
// so "warn" is signalled by ✓ + liveness=unknown rather than a third glyph.
func printCheckConflicts(stdout io.Writer, rows []checkConflict) bool {
	blocking := 0
	for i := range rows {
		if rows[i].Blocking {
			blocking++
		}
	}
	head := "✓"
	if blocking > 0 {
		head = "✗"
	}
	fmt.Fprintf(stdout, "%s conflicts count=%d blocking=%d\n", head, len(rows), blocking)
	for i := range rows {
		r := &rows[i]
		if r.Blocking {
			fmt.Fprintf(stdout, "✗ path=%s blocker=%s holder_target=%s intent=%q expires_at=%s liveness=alive\n",
				relPath(r.Path), r.Blocker.OwnerUUID, relPath(r.Blocker.Target.Canonical), r.Blocker.Intent,
				r.Blocker.ExpiresAt.UTC().Format(time.RFC3339))
			// Prefer tag+defer: the breadcrumb surfaces in the holder's tag
			// footer on their next loto command, so they can reach you directly
			// (SendMessage; `loto who` maps the handle to a peer name) instead of
			// you sitting on the lock. Force-break stays the escape hatch for a
			// dead holder. Do NOT wait in place.
			// Shell-quote the path: it lands in copy-pasted shell commands, and a
			// path with spaces or metacharacters would otherwise break or inject
			// (Codex #223 P2).
			blocker := shellQuote(relPath(r.Blocker.Target.Canonical))
			fmt.Fprintln(stdout, "```bash")
			fmt.Fprintf(stdout, "loto tag %s \"waiting — ping me on release\"  # then work elsewhere; the holder sees this in their tag footer\n", blocker)
			fmt.Fprintf(stdout, "loto unlock --force -t \"unblock\" %s  # only if the holder is dead\n", blocker)
			fmt.Fprintln(stdout, "```")
			continue
		}
		fmt.Fprintf(stdout, "✓ path=%s blocker=%s holder_target=%s intent=%q expires_at=%s liveness=unknown\n",
			relPath(r.Path), r.Blocker.OwnerUUID, relPath(r.Blocker.Target.Canonical), r.Blocker.Intent,
			r.Blocker.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return blocking > 0
}

// shellQuote wraps s in single quotes for safe interpolation into a copy-pasted
// shell command, escaping any embedded single quote as the POSIX '\” idiom. A
// path with spaces or shell metacharacters is thus inert (Codex #223 P2).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
