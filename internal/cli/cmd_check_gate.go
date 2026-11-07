package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/render"
)

// gateDecide partitions targets into deny rows: a foreign live hold that
// covers a target (its own path, or a directory above it). Pure — no IO, no clock read beyond ec.Now — so cmdCheck's
// gate branch (cmd_check.go) and this file's unit tests share one decision
// surface (gate-design.md component 4). Returns render.GateDenyRow directly
// (the precedent validateLockTargets/cmd_lock.go sets for a decision
// function building render types at the point of decision, rather than a
// private cli-local shape converted at the boundary). Deliberately diverges
// from plain check's computeCheckConflicts (plan "gate semantics vs plain
// check"):
//
//  1. any foreign live hold denies; a directory hold on internal/store
//     denies a not-yet-on-disk internal/store/new.go.
//  2. the liveness threshold is !IsStale, not Classify==Alive: a PID-0
//     hold (LivenessUnknown under Classify) must still deny within its TTL.
//
// Output is deterministically sorted path -> kind -> holder UUID -> blocker
// path (.claude/rules/design.md: same input, byte-identical output). The
// blocker-path tie-break matters: one owner can hold two directories above
// one target — same path/kind/holder, distinct rows.
func gateDecide(targets []domain.Target, locks []domain.LockRecord, myUUID string, ec domain.EvalContext) []render.GateDenyRow {
	var rows []render.GateDenyRow
	seen := map[string]bool{}
	for _, t := range targets {
		rows = appendGateDenyForTarget(rows, seen, t, locks, myUUID, ec)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		if rows[i].Kind != rows[j].Kind {
			return rows[i].Kind < rows[j].Kind
		}
		if rows[i].HolderUUID != rows[j].HolderUUID {
			return rows[i].HolderUUID < rows[j].HolderUUID
		}
		return rows[i].BlockerPath < rows[j].BlockerPath
	})
	return rows
}

// appendGateDenyForTarget scans holds for foreign live coverage of t,
// appending a deny row per distinct blocker. seen dedupes across repeated
// targets in the input (a duplicate CLI arg) the same way
// computeCheckConflicts dedupes plain check's rows. Each key includes the
// blocker path: a hold may be a directory, so one owner can hold several
// holds covering one target, each owed a row.
func appendGateDenyForTarget(rows []render.GateDenyRow, seen map[string]bool, t domain.Target, locks []domain.LockRecord, myUUID string, ec domain.EvalContext) []render.GateDenyRow {
	for i := range locks {
		l := &locks[i]
		if !gateLockDenies(t, l, myUUID, ec) {
			continue
		}
		key := "lock|" + t.Canonical + "|" + l.Target.Canonical + "|" + string(l.OwnerUUID)
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, render.GateDenyRow{
			Path: t.Canonical, Kind: render.GateKindLock, HolderUUID: string(l.OwnerUUID),
			Intent: l.Intent, ExpiresAt: l.ExpiresAt, BlockerPath: l.Target.Canonical,
		})
	}
	return rows
}

// gateLockDenies reports whether lock l is foreign live coverage of t.
func gateLockDenies(t domain.Target, l *domain.LockRecord, myUUID string, ec domain.EvalContext) bool {
	return ec.Refuses(domain.AgentUUID(myUUID), ec.MyWorktree, t.Canonical, ec.LockHold(*l))
}

// gateInfraUnreachable renders the shared infra-fail-open row: `loto check
// --gate` never blocks the caller's loop on its own IO trouble (gate-design
// "Rules: fail-open, everywhere"). Always STDERR, never stdout (loto-tzmv.8):
// a PreToolUse hook exiting 0 with stdout output does not surface that output
// to the model, so a stdout notice announces "the gate did not run" into a
// channel nobody reads. Fail-open must be loud or it is not honest. The exit
// code is unchanged — the stream moved, the verdict did not.
func gateInfraUnreachable(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "⚠ store=unreachable gate=fail-open err=%q\n", err)
	return 3
}

// runCheckGate is the IO runner for `loto check --gate <path>...`: resolve
// targets (never stats them — a directory hold covers not-yet-on-disk paths,
// the kuv.10 class), short-circuit fail-open BEFORE any store IO when identity
// isn't pinned (an unpinned identity.Ensure mints a throwaway UUID owning
// nothing, so opening the store here would false-deny every path on a bare
// human-shell invocation), then read-only query ListLocks and
// render gateDecide's verdict. Never acquires, refreshes, or chmods — pure
// read (plan CLI contract). Verdict rows (✓ no conflicts, deny rows, invalid
// rows) go to stdout; FAIL-OPEN notices go to stderr (loto-tzmv.8), because a
// hook that exits 0 after writing to stdout leaves the model blind to the fact
// that the gate never ran.
func runCheckGate(ctx context.Context, paths []string, base, repoTop string, stdout, stderr io.Writer) int {
	// Before any verdict: say so if this binary predates the hook that called
	// it (loto-tzmv.7). A stale gate answers ✓ with the confidence of a current
	// one, which is how the 2026-08-12 rot went unnoticed for 8 days.
	warnIfContractStale(stderr)

	targets, invalid := resolveCheckTargets(base, repoTop, paths)
	if len(invalid) > 0 {
		printCheckInvalid(stdout, invalid)
		return 2
	}

	// Fail OPEN before any store IO on an identity the gate can't tie to real
	// ownership: an unpinned identity.Ensure mints a throwaway owning nothing,
	// so opening the store would false-deny every path (gate-design "Rules:
	// fail-open, everywhere"). identity.PinnedByEnv is the single source of truth
	// shared with release --all's fail-CLOSED refuse and Ensure's own precedence
	// — all hinge on the same "does Ensure resolve a real owner" question, so the
	// gate probe can't drift from resolution (loto-ai5, loto-s3l).
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

	// memoized: gateDecide evaluates the predicate per (target × record), so a
	// wide staged set would otherwise re-probe one holder hundreds of times.
	ec := domain.EvalContext{Now: time.Now(), Live: memoLiveProbe(rt.liveProbe()), CaseFold: rt.CaseFold, MyWorktree: rt.Worktree}
	rows := gateDecide(targets, locks, rt.Agent.UUID, ec)
	// Resurfaced on BOTH verdicts, and never as one: an unresolved violation
	// is not a reason to block this tool call — it is a reason the NEXT
	// submit will be refused, and the agent is better told now than at the
	// end of the work. Read-only, like status: the gate's hot path does not
	// run the sensor.
	violationNotice(rt, stdout)
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "✓ no conflicts")
		return 0
	}
	render.EmitGateDeny(stdout, rows)
	return 1
}
