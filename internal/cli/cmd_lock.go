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

func cmdLock(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ttl := fs.Duration("ttl", 30*time.Minute, "lock TTL")
	intent := fs.String("t", "", "intent (required)")
	fs.StringVar(intent, "intent", "", "intent (required)")
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

	return acquireBatch(rt, targets, *intent, *ttl, rt.liveProbe(), stdout, stderr)
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

func acquireBatch(rt *runtime, targets []domain.Target, intent string, ttl time.Duration, live domain.PidLiveProbe, stdout, stderr io.Writer) int {
	now := time.Now()
	recs := buildLockRecords(targets, rt, intent, now, ttl)
	acquired, err := rt.Locks().AcquireLocks(rt.Ctx, recs, live)
	if err != nil {
		var mce *store.MultiConflictError
		if errors.As(err, &mce) {
			render.EmitConflict(stdout, mce)
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
	emitted := make([]domain.Target, len(acquired))
	for i := range acquired {
		emitted[i] = acquired[i].Target
	}
	render.EmitLockSuccess(stdout, emitted)
	return 0
}

func buildLockRecords(targets []domain.Target, rt *runtime, intent string, now time.Time, ttl time.Duration) []domain.LockRecord {
	recs := make([]domain.LockRecord, 0, len(targets))
	for _, t := range targets {
		recs = append(recs, domain.LockRecord{
			Target:      t,
			OwnerUUID:   rt.Agent.UUID,
			SessionUUID: rt.SessionUUID,
			Intent:      intent,
			CreatedAt:   now,
			ExpiresAt:   now.Add(ttl),
			Host:        rt.Host,
			PID:         stampPID(),
		})
	}
	return recs
}
