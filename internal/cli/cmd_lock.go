package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

func init() { //nolint:gochecknoinits // command registry pattern
	register("lock", cmdLock)
}

func cmdLock(args []string, stdout, stderr io.Writer) int {
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
	targets, code := canonicalizeTargets(fs.Args(), stderr)
	if code != 0 {
		return code
	}
	rt, err := openRuntime()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	emitMsgBanner(stdout, rt)

	live := func(host string, pid int) bool {
		if host != rt.Host {
			return true
		}
		return pidLive(pid)
	}
	return acquireBatch(rt, targets, *intent, *ttl, live, stdout, stderr)
}

func acquireBatch(rt *runtime, targets []domain.Target, intent string, ttl time.Duration, live func(string, int) bool, stdout, stderr io.Writer) int {
	now := time.Now()
	recs := buildLockRecords(targets, rt, intent, now, ttl)
	acquired, err := rt.Store.AcquireLocks(rt.Ctx, recs, live)
	if err != nil {
		var mce *store.MultiConflictError
		if errors.As(err, &mce) {
			emitMultiConflict(stdout, mce)
			return 1
		}
		var cfe *store.ChmodFailureError
		if errors.As(err, &cfe) {
			emitChmodFailure(stdout, cfe)
			return 3
		}
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	emitLockSuccess(stdout, rt, acquired)
	return 0
}

func canonicalizeTargets(args []string, stderr io.Writer) ([]domain.Target, int) {
	out := make([]domain.Target, 0, len(args))
	for _, a := range args {
		t, err := domain.Canonicalize(a)
		if err != nil {
			fmt.Fprintf(stderr, "✗ target %s: %v\n", a, err)
			return nil, 2
		}
		out = append(out, t)
	}
	return out, 0
}

func buildLockRecords(targets []domain.Target, rt *runtime, intent string, now time.Time, ttl time.Duration) []domain.LockRecord {
	recs := make([]domain.LockRecord, 0, len(targets))
	for _, t := range targets {
		recs = append(recs, domain.LockRecord{
			Target:      t,
			OwnerUUID:   rt.Agent.UUID,
			SessionUUID: rt.Agent.UUID,
			Intent:      intent,
			CreatedAt:   now,
			ExpiresAt:   now.Add(ttl),
			Host:        rt.Host,
			PID:         os.Getpid(),
		})
	}
	return recs
}

func emitMultiConflict(w io.Writer, mce *store.MultiConflictError) {
	fmt.Fprintf(w, "✗ blocked blockers=%d\n", len(mce.Blockers))
	for i := range mce.Blockers {
		b := &mce.Blockers[i]
		fmt.Fprintf(w, "✗ blocker=%s target=%s intent=%q held_since=%s expires_at=%s host=%s pid=%d\n",
			b.OwnerUUID, b.Target.Canonical, b.Intent,
			b.CreatedAt.UTC().Format(time.RFC3339), b.ExpiresAt.UTC().Format(time.RFC3339),
			b.Host, b.PID)
	}
}

func emitChmodFailure(w io.Writer, cfe *store.ChmodFailureError) {
	fmt.Fprintf(w, "✗ chmod_failed targets=%d\n", len(cfe.Failures))
	for i := range cfe.Failures {
		f := &cfe.Failures[i]
		fmt.Fprintf(w, "✗ target=%s rolled_back=%t err=%v\n", f.Target.Canonical, f.RolledBack, f.Err)
	}
}

func emitLockSuccess(w io.Writer, rt *runtime, acquired []domain.LockRecord) {
	fmt.Fprintf(w, "✓ locked count=%d\n", len(acquired))
	for i := range acquired {
		t := acquired[i].Target
		fmt.Fprintf(w, "✓ locked target=%s\n", t.Canonical)
		tags, _ := rt.Store.UnreadTagsForAddressee(rt.Ctx, rt.Agent.UUID, t)
		for j := range tags {
			tg := &tags[j]
			fmt.Fprintf(w, "✓ tag=%s intent=%q\n", tg.ID, strings.TrimSpace(tg.Intent))
		}
		if len(tags) > 0 {
			_ = rt.Store.MarkRead(rt.Ctx, rt.Agent.UUID, t)
		}
	}
}

func emitMsgBanner(w io.Writer, rt *runtime) {
	count, senders, err := rt.Store.UnreadMessageSummary(rt.Ctx, rt.Agent.UUID)
	if err != nil || count == 0 {
		return
	}
	names := make([]string, 0, len(senders))
	for _, s := range senders {
		names = append(names, shortenUUID(s))
	}
	fmt.Fprintf(w, "ℹ %d msg for you from %s — loto msg to read\n", count, strings.Join(names, ", "))
}

func shortenUUID(uuid string) string {
	if len(uuid) > 8 {
		return uuid[:8]
	}
	return uuid
}
