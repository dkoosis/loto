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
	code := 0
	for _, arg := range fs.Args() {
		targets, resolveErr := resolveTargets(arg)
		if resolveErr != nil {
			fmt.Fprintf(stderr, "✗ target %q: %v\n", arg, resolveErr)
			code = 2
			continue
		}
		for _, target := range targets {
			if c := acquireOne(rt, target, *intent, *ttl, live, stdout, stderr); c != 0 {
				code = c
			}
		}
	}
	return code
}

func acquireOne(rt *runtime, target domain.Target, intent string, ttl time.Duration, live func(string, int) bool, stdout, stderr io.Writer) int {
	now := time.Now()
	rec := domain.LockRecord{
		Target:      target,
		OwnerUUID:   rt.Agent.UUID,
		SessionUUID: rt.Agent.UUID,
		Intent:      intent,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
		Host:        rt.Host,
		PID:         os.Getpid(),
	}
	_, err := rt.Store.AcquireLock(rt.Ctx, rec, live)
	if err != nil {
		var ce *store.ConflictError
		if errors.As(err, &ce) {
			emitConflict(stdout, ce)
			return 1
		}
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	emitLockSuccess(stdout, rt, target)
	return 0
}

func emitConflict(w io.Writer, ce *store.ConflictError) {
	if len(ce.Blockers) == 0 {
		return
	}
	fmt.Fprintf(w, "✗ blocked target=%s\n", ce.Blockers[0].Target.Canonical)
	for i := range ce.Blockers {
		b := &ce.Blockers[i]
		fmt.Fprintf(w, "⚠ blocker=%s target=%s intent=%q held_since=%s expires_at=%s host=%s pid=%d\n",
			b.OwnerUUID, b.Target.Canonical, b.Intent,
			b.CreatedAt.UTC().Format(time.RFC3339), b.ExpiresAt.UTC().Format(time.RFC3339),
			b.Host, b.PID)
	}
}

func emitLockSuccess(w io.Writer, rt *runtime, t domain.Target) {
	fmt.Fprintf(w, "✓ locked target=%s\n", t.Canonical)
	tags, _ := rt.Store.UnreadTagsForAddressee(rt.Ctx, rt.Agent.UUID, t)
	for i := range tags {
		tg := &tags[i]
		fmt.Fprintf(w, "ℹ tag=%s intent=%q\n", tg.ID, strings.TrimSpace(tg.Intent))
	}
	if len(tags) > 0 {
		_ = rt.Store.MarkRead(rt.Ctx, rt.Agent.UUID, t)
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
