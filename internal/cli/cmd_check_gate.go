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
		if !domain.ClaimCoversTarget(*c, t.Canonical, myUUID, ec.Now) {
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

// gateInfraUnreachable renders the shared infra-fail-open row: `loto check
// --gate` never blocks the caller's loop on its own IO trouble (gate-design
// "Rules: fail-open, everywhere"). Always stdout, never stderr — the CLI
// contract (plan) pins this row to stdout for both the store-open failure
// and any subsequent store read failure.
func gateInfraUnreachable(stdout io.Writer, err error) int {
	fmt.Fprintf(stdout, "⚠ store=unreachable gate=fail-open err=%q\n", err)
	return 3
}

// runCheckGate is the IO runner for `loto check --gate <path>...`: resolve
// targets (never stats them — a claim covers not-yet-on-disk paths, the
// kuv.10 class), short-circuit fail-open BEFORE any store IO when identity
// isn't pinned (an unpinned identity.Ensure mints a throwaway UUID owning
// nothing, so opening the store here would false-deny every path on a bare
// human-shell invocation), then read-only query ListLocks/ListClaims and
// render gateDecide's verdict. Never acquires, refreshes, or chmods — pure
// read (plan CLI contract). All output goes to stdout, including invalid/
// infra rows, so a hook consuming this surface has one stream to read.
func runCheckGate(ctx context.Context, paths []string, repoTop string, stdout io.Writer) int {
	targets, invalid := resolveCheckTargets(repoTop, paths)
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
		fmt.Fprintln(stdout, "⚠ identity=unpinned gate=fail-open")
		return 0
	}

	rt, err := openRuntime(ctx)
	if err != nil {
		return gateInfraUnreachable(stdout, err)
	}
	defer rt.Close()

	locks, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		return gateInfraUnreachable(stdout, err)
	}
	claims, err := rt.Store.ListClaims(rt.Ctx)
	if err != nil {
		return gateInfraUnreachable(stdout, err)
	}

	ec := domain.EvalContext{Now: time.Now(), ThisHost: rt.Host, Live: rt.liveProbe()}
	rows := gateDecide(targets, locks, claims, rt.Agent.UUID, ec)
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "✓ no conflicts")
		return 0
	}
	render.EmitGateDeny(stdout, rows)
	return 1
}
