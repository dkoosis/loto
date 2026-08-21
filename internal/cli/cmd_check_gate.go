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

// gateDecide partitions targets into deny rows: a foreign live claim whose
// prefix overlaps a target, or a foreign live lock/beacon on a target's
// exact path. Pure — no IO, no clock read beyond ec.Now — so cmdCheck's
// gate branch (cmd_check.go) and this file's unit tests share one decision
// surface (gate-design.md component 4). Returns render.GateDenyRow directly
// (the precedent validateLockTargets/cmd_lock.go sets for a decision
// function building render types at the point of decision, rather than a
// private cli-local shape converted at the boundary). Deliberately diverges
// from plain check's computeCheckConflicts (plan "gate semantics vs plain
// check"):
//
//  1. any foreign live lock/beacon denies, any mode — not the shared-vs-
//     shared-safe probe computeCheckConflicts uses.
//  2. claims are consulted at all (plain check never reads them);
//     PrefixOverlaps gives ancestor coverage, so a claim on internal/store
//     denies a not-yet-on-disk internal/store/new.go.
//  3. the liveness threshold is !IsStale, not Classify==Alive: a hook-
//     minted beacon carries PID-0 (LivenessUnknown under Classify) and must
//     still deny within its TTL, or the beacon leg of the gate no-ops.
//
// Output is deterministically sorted path -> kind -> holder UUID -> blocker
// path (.claude/rules/design.md: same input, byte-identical output). The
// blocker-path tie-break matters: one owner can hold claims at two ancestor
// prefixes of one target — same path/kind/holder, distinct rows.
func gateDecide(targets []domain.Target, locks []domain.LockRecord, claims []domain.ClaimRecord, myUUID string, ec domain.EvalContext) []render.GateDenyRow {
	var rows []render.GateDenyRow
	seen := map[string]bool{}
	for _, t := range targets {
		rows = appendGateDenyForTarget(rows, seen, t, locks, claims, myUUID, ec)
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

// appendGateDenyForTarget scans locks then claims for foreign live coverage
// of t, appending a deny row per distinct (kind, blocker) pair. seen dedupes
// across repeated targets in the input (a duplicate CLI arg) the same way
// computeCheckConflicts dedupes plain check's rows. The lock key omits the
// blocker path — it always equals t.Canonical (locks are per-file) — while
// the claim key includes c.PathPrefix: two foreign claims at different
// ancestor prefixes of one target are distinct blockers, each owed a row.
func appendGateDenyForTarget(rows []render.GateDenyRow, seen map[string]bool, t domain.Target, locks []domain.LockRecord, claims []domain.ClaimRecord, myUUID string, ec domain.EvalContext) []render.GateDenyRow {
	for i := range locks {
		l := &locks[i]
		if !domain.SameCanonical(t, l.Target) || string(l.OwnerUUID) == myUUID || ec.IsStale(*l) {
			continue
		}
		key := "lock|" + t.Canonical + "|" + string(l.OwnerUUID)
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, render.GateDenyRow{
			Path: t.Canonical, Kind: render.GateKindLock, HolderUUID: string(l.OwnerUUID),
			Intent: l.Intent, ExpiresAt: l.ExpiresAt, BlockerPath: l.Target.Canonical,
		})
	}
	for i := range claims {
		c := &claims[i]
		// ClaimCoversTarget settles overlap + foreign + TTL; ec.ClaimIsStale adds
		// the owner-liveness leg so the path-scoped gate and the repo-wide
		// gateDecideAny apply one standard of "live" to claims (loto-tzmv.9).
		if !domain.ClaimCoversTarget(*c, t.Canonical, myUUID, ec.Now) || ec.ClaimIsStale(*c) {
			continue
		}
		key := "claim|" + t.Canonical + "|" + c.PathPrefix + "|" + string(c.OwnerUUID)
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, render.GateDenyRow{
			Path: t.Canonical, Kind: render.GateKindClaim, HolderUUID: string(c.OwnerUUID),
			Intent: c.Intent, ExpiresAt: c.ExpiresAt, BlockerPath: c.PathPrefix,
		})
	}
	return rows
}

// gateDecideAny is gateDecide's path-free sibling (ccp-vx4w): "does any
// foreign live lock or claim exist anywhere in this repo", for a guard whose
// operation (a branch switch) has no meaningful path operand — unlike
// gateDecide, it never matches a target, it enumerates. One deny row per
// distinct foreign live lock and per distinct foreign live claim; the
// per-target dedupe keys collapse to lock/claim identity alone since there
// is no target path to key on. Same liveness rule as gateDecide (!IsStale,
// not Classify==Alive) and the same wave carve-out (myUUID excluded) —
// deliberately reuses gateDecide's semantics rather than drifting a second
// definition of "foreign" and "live".
func gateDecideAny(locks []domain.LockRecord, claims []domain.ClaimRecord, myUUID string, mySession domain.SessionUUID, ec domain.EvalContext) []render.GateDenyRow {
	var rows []render.GateDenyRow
	for i := range locks {
		l := &locks[i]
		if string(l.OwnerUUID) == myUUID || ec.IsStale(*l) {
			continue
		}
		// A beacon minted by a SIBLING of this same Claude session does not
		// refuse this session's tree-move (loto-xwod). Subagents of one session
		// are distinct loto owners by design — that is exactly what lets one
		// sibling's beacon deny another sibling's WRITE — but they are one
		// session as far as `git checkout` is concerned, and a session has to be
		// able to move the tree it is working in.
		//
		// Narrow on purpose: only beacons, and only same-session ones. A
		// sibling's real exclusive lock still refuses the move, because a
		// checkout under declared uncommitted territory is the 2026-08-14
		// incident itself. An empty mySession (no CLAUDE_CODE_SESSION_ID, direct
		// CLI use) matches nothing rather than everything.
		if l.IsBeacon() && mySession != "" && l.SessionUUID == mySession {
			continue
		}
		rows = append(rows, render.GateDenyRow{
			Path: l.Target.Canonical, Kind: render.GateKindLock, HolderUUID: string(l.OwnerUUID),
			Intent: l.Intent, ExpiresAt: l.ExpiresAt, BlockerPath: l.Target.Canonical,
		})
	}
	for i := range claims {
		c := &claims[i]
		// loto-tzmv.9: claims get the SAME liveness standard locks get above.
		// Filtering on Expired alone let a crashed session's claim deny every
		// tree-move in the repo for its full 2h TTL, with no reclaim path.
		if string(c.OwnerUUID) == myUUID || ec.ClaimIsStale(*c) {
			continue
		}
		rows = append(rows, render.GateDenyRow{
			Path: c.PathPrefix, Kind: render.GateKindClaim, HolderUUID: string(c.OwnerUUID),
			Intent: c.Intent, ExpiresAt: c.ExpiresAt, BlockerPath: c.PathPrefix,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		if rows[i].Kind != rows[j].Kind {
			return rows[i].Kind < rows[j].Kind
		}
		return rows[i].HolderUUID < rows[j].HolderUUID
	})
	return rows
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
// targets (never stats them — a claim covers not-yet-on-disk paths, the
// kuv.10 class), short-circuit fail-open BEFORE any store IO when identity
// isn't pinned (an unpinned identity.Ensure mints a throwaway UUID owning
// nothing, so opening the store here would false-deny every path on a bare
// human-shell invocation), then read-only query ListLocks/ListClaims and
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
	claims, err := rt.Store.ListClaims(rt.Ctx)
	if err != nil {
		return gateInfraUnreachable(stderr, err)
	}

	// memoized: gateDecide evaluates the predicate per (target × record), so a
	// wide staged set would otherwise re-probe one holder hundreds of times.
	ec := domain.EvalContext{Now: time.Now(), Live: memoLiveProbe(rt.liveProbe())}
	rows := gateDecide(targets, locks, claims, rt.Agent.UUID, ec)
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
