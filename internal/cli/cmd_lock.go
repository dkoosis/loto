package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/render"
	"loto/internal/store"
)

func init() { //nolint:gochecknoinits // command registry pattern
	register("lock", cmdLock)
}

// reasonNotRegularFile is the design.md token for "this path is not a regular
// file". Three surfaces emit or read it — the Lstat check, the ErrTargetIsDir
// mapping, and emitDirLockHint's filter — and a typo in any one of them makes
// the hint silently stop firing, which reads exactly like "no directory was
// passed" (.claude/rules/standard-checks.md).
const reasonNotRegularFile = "not-regular-file"

// lockUsageHead is the point-of-use teaching surface for lock (loto-5rwc):
// usage line plus worked examples. The flag list is appended by PrintDefaults.
const lockUsageHead = `usage: loto lock <target> [<target>...] -t "why" [--shared]

Acquire a lock on one or more targets. -t (intent) is required.
Default mode is exclusive (sole writer). --shared takes a multi-reader lease.

examples:
  loto lock internal/store/store.go -t "store refactor"
  loto lock README.md -t "reading docs" --shared
`

func cmdLock(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, lockUsageHead)
		fs.PrintDefaults()
	}
	ttl := fs.Duration("ttl", domain.DegradedModeTTL, "lock TTL")
	intent := fs.String("t", "", "intent (required)")
	fs.StringVar(intent, "intent", "", "intent (required)")
	shared := fs.Bool("shared", false, "acquire a shared (multi-reader) lock; default is exclusive")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if *intent == "" {
		fmt.Fprintln(stderr, "✗ -t required: loto lock <target> [<target>...] -t \"why\"")
		return 2
	}
	// A non-positive TTL mints an instantly-stale lock: the exclusive strip
	// leaves the file read-only under a lease any peer may reclaim at once.
	// Reject up front — claim parity (cmd_claim.go).
	if *ttl <= 0 {
		fmt.Fprintf(stderr, "✗ --ttl must be positive, got %s\n", *ttl)
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: loto lock <target> [<target>...] -t \"why\"")
		return 2
	}
	repoTop, _ := repoTopForCwd(ctx)
	targets, invalid := validateLockTargets(fs.Args(), repoTop, false)
	if len(invalid) > 0 {
		render.EmitInvalid(stderr, invalid)
		emitDirLockHint(stderr, invalid, fs.Args(), repoTop, *intent)
		return 2
	}
	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	defer rt.DeferredTagFooter(stdout)

	mode := domain.ModeExclusive
	if *shared {
		mode = domain.ModeShared
	}
	return acquireBatch(rt, targets, *intent, *ttl, mode, rt.liveProbe(), stdout, stderr)
}

// emitDirLockHint — sd-hh0h. `loto lock <dir>` is refused with
// reason=not-regular-file, which says WHY and never WHAT TO RUN INSTEAD. Two
// lanes read that refusal on 2026-09-05 and edited unlocked: the gate was
// simply off, and nothing in the output said so. The same two round trips were
// recorded a month earlier (kg 356affa894c2), so the refusal's silence is the
// defect, not the refusal.
//
// ‡ The refusal STANDS — option B on the bead. loto has two nouns by design
// (loto-qoq, loto-7af9): `claim <prefix>` reserves TERRITORY, a directory,
// advisory between claimants; `lock <file>...` takes the WRITE-SET, regular
// files, blocking. Making lock expand a directory (option A) would grow a
// lock's territory with the tree, which is "lock the write-set, not the blast
// radius" inverted. So the fix is that the refusal names both replacements.
//
// The directory test itself lives in refusedDirTargets below.
func emitDirLockHint(w io.Writer, invalid []render.InvalidTarget, args []string, repoTop, intent string) {
	dirs := refusedDirTargets(invalid, args, repoTop)
	if len(dirs) == 0 {
		return
	}
	why := intent
	if why == "" {
		why = "<bead>: intent"
	}
	fmt.Fprintln(w, "ℹ a directory is not a lock target — claim the territory, lock the files:")
	for _, d := range dirs {
		fmt.Fprintf(w, "ℹ   loto claim %s -t %q\n", d, why)
		fmt.Fprintf(w, "ℹ   loto lock $(fd -t f . %s) -t %q\n", d, why)
	}
	fmt.Fprintln(w, "ℹ claim reserves the PREFIX (advisory between claimants); lock takes the WRITE-SET (regular files, blocking).")
}

// refusedDirTargets picks the refused paths that are DIRECTORIES, in input
// order, deduplicated.
//
// ‡ A directory, not merely a non-regular file. A fifo, socket or device also
// answers not-regular-file, and neither replacement line is the answer for one
// — printing them would teach a wrong move at exactly the moment the caller is
// looking for the right one. Two arms, because the two spellings reach the
// refusal by different routes: a bare directory fails statFileTargetReason's
// IsRegular check and stats as a dir here; a trailing-slash one is refused by
// domain.ErrTargetIsDir BEFORE any stat, and only a directory is ever written
// that way.
func refusedDirTargets(invalid []render.InvalidTarget, args []string, repoTop string) []string {
	slashed := make(map[string]bool, len(args))
	for _, a := range args {
		if strings.HasSuffix(a, "/") && len(a) > 1 {
			slashed[a] = true
		}
	}
	seen := make(map[string]bool, len(invalid))
	dirs := make([]string, 0, len(invalid))
	for _, it := range invalid {
		if it.Reason != reasonNotRegularFile {
			continue
		}
		d := strings.TrimRight(it.Path, "/")
		if d == "" || seen[d] || (!slashed[it.Path] && !statIsDir(repoTop, d)) {
			continue
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	return dirs
}

// statIsDir resolves a repo-relative path against repoTop the same way
// statFileTargetReason does — statting it bare would mean
// "repoTop/<cwd-suffix>/<path>" whenever loto runs from a subdirectory.
func statIsDir(repoTop, rel string) bool {
	probe := rel
	if repoTop != "" {
		probe = filepath.Join(repoTop, rel)
	}
	st, err := os.Stat(probe)
	return err == nil && st.IsDir()
}

// validateLockTargets canonicalizes and Lstat-validates each path before any
// store work, so rejection produces a single render.EmitInvalid block and
// leaves zero side effects on disk or DB.
//
// allowMissing tolerates ENOENT instead of rejecting it — the beacon-scoped
// carve-out (loto-z5nb): `loto beacon` announces a write about to happen, and
// a Write tool call creating a brand-new path is exactly the case a beacon
// exists to protect. domain.Canonicalize (inside resolveCLITarget) already
// tolerates a non-existent path; this was the one place downstream that still
// demanded the file pre-exist. Every other check — symlink, non-regular —
// still runs unconditionally when the path DOES exist, and `loto lock` itself
// passes false, unchanged.
func validateLockTargets(args []string, repoTop string, allowMissing bool) ([]domain.Target, []render.InvalidTarget) {
	targets := make([]domain.Target, 0, len(args))
	seen := make(map[string]bool, len(args))
	var invalid []render.InvalidTarget
	// Every token here is caller-typed, so one syscall serves the whole batch.
	base := callerBase()
	cc := newCaseCache()
	for _, raw := range args {
		t, err := resolveCLITarget(cc, base, repoTop, raw)
		if err != nil {
			invalid = append(invalid, render.InvalidTarget{Path: raw, Reason: classifyCanonicalizeErr(err)})
			continue
		}
		if seen[t.Canonical] {
			invalid = append(invalid, render.InvalidTarget{Path: t.Canonical, Reason: "duplicate-target"})
			continue
		}
		seen[t.Canonical] = true
		if reason := statFileTargetReason(repoTop, t.Canonical, allowMissing); reason != "" {
			invalid = append(invalid, render.InvalidTarget{Path: t.Canonical, Reason: reason})
			continue
		}
		targets = append(targets, t)
	}
	return targets, invalid
}

// statFileTargetReason runs the Lstat-shaped half of target validation for
// one already-canonicalized, already-deduped path. Empty string is a clean
// pass (which the allowMissing carve-out also produces, on ENOENT); any other
// value names why the caller should reject it. Split out from
// validateLockTargets purely to keep that loop's branching under the
// complexity gate (gocognit) — no behavior change from the inline version it
// replaces.
//
// ‡ canonical is REPO-relative (resolveCLITarget -> normalizeRepoPath), so it
// must be stat'd under repoTop, not against the process CWD. Statting it bare
// silently means "repoTop/<cwd-suffix>/<canonical>" whenever loto runs from a
// subdirectory — which used to surface as a spurious `not-found` on `loto
// lock`, and with the allowMissing carve-out below would instead SILENTLY
// admit a beacon while skipping the symlink and regular-file checks the path
// would actually have failed (Codex #258 P1). repoTop is empty only outside a
// git repo, where CWD-relative is the only meaning available.
func statFileTargetReason(repoTop, canonical string, allowMissing bool) string {
	probe := canonical
	if repoTop != "" {
		probe = filepath.Join(repoTop, canonical)
	}
	lst, err := os.Lstat(probe)
	if err != nil {
		if allowMissing && errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		if errors.Is(err, fs.ErrNotExist) {
			return "not-found"
		}
		return "stat-failed: " + err.Error()
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return "symlink"
	}
	if !lst.Mode().IsRegular() {
		return reasonNotRegularFile
	}
	return ""
}

// classifyCanonicalizeErr maps domain errors — and the one resolver error that
// can precede them — to design.md reason tokens.
func classifyCanonicalizeErr(err error) string {
	switch {
	case errors.Is(err, errCallerCWDUnknown):
		// The same fact refuseUnresolvableRelative reports through a different
		// door: the caller's cwd is not knowable, so a relative token has no
		// base. Deliberately reuses that token rather than minting a second one.
		return "relative-path-caller-cwd-unknown"
	case errors.Is(err, domain.ErrTargetIsDir):
		return reasonNotRegularFile
	case errors.Is(err, domain.ErrEmptyTarget):
		return "empty-target"
	case errors.Is(err, domain.ErrTargetHasNUL):
		return "target-has-nul"
	case errors.Is(err, domain.ErrTargetBackslash):
		return "target-has-backslash"
	case errors.Is(err, domain.ErrRepoEscape):
		return "repo-escape"
	case errors.Is(err, domain.ErrTargetIsGlob):
		return "glob-not-supported"
	case errors.Is(err, domain.ErrTargetUnspellable):
		return "not-a-path"
	case errors.Is(err, domain.ErrTargetIsRepoRoot):
		return "repo-root"
	default:
		return err.Error()
	}
}

func acquireBatch(rt *runtime, targets []domain.Target, intent string, ttl time.Duration, mode string, live domain.HolderLiveProbe, stdout, stderr io.Writer) int {
	now := time.Now()
	if w := degradedPidWarning(); w != "" {
		fmt.Fprint(stderr, w)
	}
	recs := buildLockRecords(targets, rt, intent, now, ttl, mode)
	acquired, err := rt.Store.AcquireLocks(rt.Ctx, recs, live)
	if err != nil {
		var mce *store.MultiConflictError
		if errors.As(err, &mce) {
			render.EmitConflictWithTags(stdout, mce, fetchTagsForBlockers(rt, mce.Blockers))
			return 1
		}
		// A candidate claim conflict is a distinct blocker shape (loto-u2p7): no
		// live lock to point at, but a pending candidate this write-set would
		// step on. Named separately so the refusal line carries the candidate
		// id, its session, and its age rather than falling to the bare
		// CandidateClaimConflictError.Error() string below.
		var ccce *store.CandidateClaimConflictError
		if errors.As(err, &ccce) {
			render.EmitCandidateClaimConflict(stdout, ccce, now)
			return 1
		}
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	render.EmitLockSuccess(stdout, acquired)
	// Foreign-claim advisory (loto-qoq): claims never block a lock, so this
	// only runs on the success path, after the success rows.
	emitForeignClaimAdvisories(stdout, foreignClaimAdvisoriesFor(rt, targets, now))
	return 0
}

// fetchTagsForBlockers returns a map keyed by target_canonical of pending tags
// on each blocker. One batched query (not per-target N+1). Read errors are
// swallowed silently — surfacing tags is best-effort and must not mask the
// underlying conflict report.
func fetchTagsForBlockers(rt *runtime, blockers []domain.LockRecord) map[string][]store.Tag {
	canonicals := make([]domain.Canonical, 0, len(blockers))
	seen := make(map[string]bool, len(blockers))
	for i := range blockers {
		c := blockers[i].Target.Canonical
		if seen[c] {
			continue
		}
		seen[c] = true
		canonicals = append(canonicals, domain.Canonical(c))
	}
	out, err := rt.Store.ListAliveByTargets(rt.Ctx, canonicals)
	if err != nil {
		return map[string][]store.Tag{}
	}
	return out
}

func buildLockRecords(targets []domain.Target, rt *runtime, intent string, now time.Time, ttl time.Duration, mode string) []domain.LockRecord {
	pid, src := stampPID()
	// A durable pid (LOTO_PID = the session process) lets a later liveness probe
	// fast-reclaim this lock when the holder dies and detect PID reuse via the
	// start-time (loto-kwlp). Without it, stamping the one-shot CLI's own pid
	// would make the lock instantly reclaimable (loto-t1tq); pid stays 0 and we
	// skip the start-time read so liveness degrades to the TTL lease (loto-j1bo).
	var procStartVal int64
	if src == pidDurable {
		procStartVal, _ = identity.ProcStart(pid)
	}
	branch := gitBranch(rt.Ctx)
	recs := make([]domain.LockRecord, 0, len(targets))
	for _, t := range targets {
		recs = append(recs, domain.LockRecord{
			Target:      t,
			OwnerUUID:   domain.AgentUUID(rt.Agent.UUID),
			SessionUUID: rt.SessionUUID,
			Intent:      intent,
			CreatedAt:   now,
			ExpiresAt:   now.Add(ttl),
			Host:        rt.Host,
			PID:         pid,
			ProcStart:   procStartVal,
			Branch:      branch,
			Mode:        mode,
		})
	}
	return recs
}
