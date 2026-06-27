package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"loto/internal/domain"
	"loto/internal/render"
	"loto/internal/store"
)

func init() { //nolint:gochecknoinits // command registry pattern
	register("lock", cmdLock)
}

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
	ttl := fs.Duration("ttl", 30*time.Minute, "lock TTL")
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
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: loto lock <target> [<target>...] -t \"why\"")
		return 2
	}
	repoTop, _ := repoTopForCwd(ctx)
	targets, invalid := validateLockTargets(fs.Args(), repoTop)
	if len(invalid) > 0 {
		render.EmitInvalid(stderr, invalid)
		return 2
	}
	rt, err := openRuntime(ctx)
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

// validateLockTargets canonicalizes and Lstat-validates each path before any
// store work, so rejection produces a single render.EmitInvalid block and
// leaves zero side effects on disk or DB.
func validateLockTargets(args []string, repoTop string) ([]domain.Target, []render.InvalidTarget) {
	targets := make([]domain.Target, 0, len(args))
	seen := make(map[string]bool, len(args))
	var invalid []render.InvalidTarget
	for _, raw := range args {
		t, err := resolveCLITarget(repoTop, raw)
		if err != nil {
			invalid = append(invalid, render.InvalidTarget{Path: raw, Reason: classifyCanonicalizeErr(err)})
			continue
		}
		if seen[t.Canonical] {
			invalid = append(invalid, render.InvalidTarget{Path: t.Canonical, Reason: "duplicate-target"})
			continue
		}
		seen[t.Canonical] = true
		lst, err := os.Lstat(t.Canonical)
		if err != nil {
			reason := "stat-failed: " + err.Error()
			if errors.Is(err, fs.ErrNotExist) {
				reason = "not-found"
			}
			invalid = append(invalid, render.InvalidTarget{Path: t.Canonical, Reason: reason})
			continue
		}
		if lst.Mode()&os.ModeSymlink != 0 {
			invalid = append(invalid, render.InvalidTarget{Path: t.Canonical, Reason: "symlink"})
			continue
		}
		if !lst.Mode().IsRegular() {
			invalid = append(invalid, render.InvalidTarget{Path: t.Canonical, Reason: "not-regular-file"})
			continue
		}
		targets = append(targets, t)
	}
	return targets, invalid
}

// classifyCanonicalizeErr maps domain errors to design.md reason tokens.
func classifyCanonicalizeErr(err error) string {
	switch {
	case errors.Is(err, domain.ErrTargetIsDir):
		return "not-regular-file"
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
	case errors.Is(err, domain.ErrTargetIsRepoRoot):
		return "repo-root"
	default:
		return err.Error()
	}
}

func acquireBatch(rt *runtime, targets []domain.Target, intent string, ttl time.Duration, mode string, live domain.PidLiveProbe, stdout, stderr io.Writer) int {
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
		var cfe *store.ChmodFailureError
		if errors.As(err, &cfe) {
			render.EmitChmodFailure(stdout, cfe)
			return 3
		}
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	render.EmitLockSuccess(stdout, acquired)
	return 0
}

// fetchTagsForBlockers returns a map keyed by target_canonical of pending tags
// on each blocker. One batched query (not per-target N+1). Read errors are
// swallowed silently — surfacing tags is best-effort and must not mask the
// underlying conflict report.
func fetchTagsForBlockers(rt *runtime, blockers []domain.LockRecord) map[string][]store.Tag {
	canonicals := make([]string, 0, len(blockers))
	seen := make(map[string]bool, len(blockers))
	for i := range blockers {
		c := blockers[i].Target.Canonical
		if seen[c] {
			continue
		}
		seen[c] = true
		canonicals = append(canonicals, c)
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
		procStartVal, _ = procStart(pid)
	}
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
			Mode:        mode,
		})
	}
	return recs
}
