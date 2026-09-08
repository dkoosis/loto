package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/render"
	"loto/internal/store"
)

func init() { register("beacon", cmdBeacon) } //nolint:gochecknoinits // command registry pattern

// beaconTTL is how long a beacon speaks for its holder without a refresh.
//
// Short on purpose. A beacon is minted by the PreToolUse gate on the holder's
// behalf, so nothing releases it — the holding agent has no idea it exists and
// there is no unlock in its future. The TTL IS the release (loto-xwod AC: "an
// ended sibling cannot wedge a path"). Two minutes covers the gap between one
// agent's edits to a file while making a finished sibling's residue expire
// before a human would notice it.
//
// The refresh is free: the gate re-mints on the holder's next write to the same
// path, and a same-owner re-acquire is an in-place TTL update (insertOrRefresh).
const beaconTTL = 2 * time.Minute

// beaconIntent is stamped on every beacon so `loto status` and a blocked peer's
// conflict rows say where the row came from. A beacon has no human author to
// ask for an intent, so the verb supplies one rather than making it optional.
const beaconIntent = "beacon: agent is writing this file"

const beaconUsageHead = `usage: loto beacon <target> [<target>...]

Mint a short-TTL shared lease on paths this agent is about to write. Minted by
the PreToolUse gate, not by hand: it makes an agent's in-flight writes visible
to peers that never ran 'loto lock'.

Shared mode, no PID, ` + "`2m`" + ` TTL, refreshed in place on re-mint. Two beacons
never block each other at acquire — 'loto check --gate' is what denies a
FOREIGN beacon's path, so a peer is stopped at its own next write.

examples:
  loto beacon internal/store/locks.go
`

// resolveBeaconGroups validates every group's targets, prints the refused ones,
// and reports how many targets survived across all groups.
//
// ‡ loto-ngip: a refused target must not sink the whole call. The store
// accumulated 33 dead lock rows keyed on tokens like "pinned"/"test"/"to" that
// were never paths (a sentence split on whitespace and handed to this verb one
// word per arg) — but the bug was not that they got validated as junk, it was
// that a beacon MIXING one such token with a real path used to drop the real
// path too: this validator already runs the same statFileTargetReason predicate
// `loto lock` applies (just with ENOENT tolerated, per z5nb), so the refused
// token is already caught here. Print it and its reason, then keep going with
// what validated — exactly what `loto lock` deliberately does NOT do, because a
// lock call is a caller's explicit typed request, not an automated announcement
// racing a write that is happening either way.
func resolveBeaconGroups(groups []beaconGroup, stderr io.Writer) (resolved []resolvedBeaconGroup, valid int) {
	resolved = make([]resolvedBeaconGroup, 0, len(groups))
	var invalid []render.InvalidTarget
	for _, g := range groups {
		targets, inv := validateLockTargets(g.rawArgs, g.repoTop, true)
		invalid = append(invalid, inv...)
		valid += len(targets)
		resolved = append(resolved, resolvedBeaconGroup{group: g, targets: targets})
	}
	if len(invalid) > 0 {
		render.EmitInvalid(stderr, invalid)
	}
	return resolved, valid
}

// cmdBeacon mints the shared, PID-less, short-TTL lease the gate reads as
// "some agent is writing here right now" (loto-xwod).
//
// The problem it closes: two subagents dispatched onto one bead share their
// parent's LOTO_AGENT_ID, and a lock never blocks its own owner (loto-fs84),
// so loto could not see sibling agents at all. On 2026-08-14 two siblings wrote
// the same two files concurrently, both holding locks, neither blocked; one
// agent's uncommitted work was then destroyed by a branch cut and survived only
// because the supervisor had snapshotted it by hand. loto's whole value
// proposition is that this cannot happen.
//
// identity.resolveSubagent already derives a stable per-sibling identity from the
// stamped LOTO_SUBAGENT_ID, so siblings arrive here as distinct owners. What was
// missing is a row for them to collide on: agents write through Edit/Write, not
// through `loto lock`, so absent a beacon there is nothing in the store to see.
//
// ‡ Shared mode, deliberately. Two siblings' beacons must NOT conflict at
// acquire — B's write is refused by the GATE reading A's beacon, one step
// earlier and with a better message, and if minting itself could fail on a peer
// the gate would be handing out denials from inside the hook's fail-open path.
// An exclusive lock still conflicts with a beacon (domain.Conflicts truth
// table), which is what denies a sibling's `loto lock` at lock time rather than
// at write time.
//
// ‡ openRuntime, NOT openRuntimeGC. This runs on the PreToolUse hot path, once
// per gated write. openRuntimeGC walks every ~/.loto/session/*.json, which cost
// 1.1s against a 15.5k-file directory — the exact scan loto-6pn6 took off the
// gate path. Beacons are self-expiring by TTL, so they accrete nothing that GC
// would need to collect.
func cmdBeacon(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("beacon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, beaconUsageHead)
		fs.PrintDefaults()
	}
	ttl := fs.Duration("ttl", beaconTTL, "beacon TTL")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if *ttl <= 0 {
		fmt.Fprintf(stderr, "✗ --ttl must be positive, got %s\n", *ttl)
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprint(stderr, beaconUsageHead)
		return 2
	}

	callerRepoTop, _ := repoTopForCwd(ctx)
	// loto-72i: a target may not live in the project the caller stands in at
	// all — a lease on it belongs to the project that OWNS the path. Group
	// before any validation so a path in no known project is refused by name
	// and never silently folded into the caller's own group.
	groups, invalidProject := groupTargetsByOwningProject(ctx, fs.Args(), callerBase(), callerRepoTop)
	if len(invalidProject) > 0 {
		render.EmitInvalid(stderr, invalidProject)
		return 2
	}

	// loto-z5nb: a beacon may name a path that does not exist yet — announcing
	// a Write about to CREATE it is the case a beacon exists to protect.
	resolved, valid := resolveBeaconGroups(groups, stderr)
	// Nothing validated ANYWHERE — across every group, not just one of them.
	// A call whose targets were all junk announces no write, and reporting
	// success for it would be the silent half of this same bug.
	if valid == 0 {
		return 2
	}

	// A beacon is a lease with an owner; an unpinned one would be a row nobody
	// can release. Refuse on the same env read openRuntimeGC uses (loto-jnid).
	// openRuntime rather than openRuntimeGC because this is the PreToolUse hot
	// path and must not pay the session GC sweep.
	if !identity.PinnedByEnv() {
		fmt.Fprintf(stderr, "✗ %v\n", errIdentityUnpinned)
		return 3
	}

	now := time.Now()
	worst := 0
	for _, rg := range resolved {
		code := acquireBeaconGroup(ctx, rg, now, *ttl, stdout, stderr)
		if code > worst {
			worst = code
		}
	}
	return worst
}

// resolvedBeaconGroup pairs a beaconGroup with its already-canonicalized,
// already-validated targets — split from groupTargetsByOwningProject's output
// so every group's targets are validated (and any invalid path reported)
// BEFORE any group's store is opened, matching validateLockTargets' own
// contract of zero side effects on a rejection.
type resolvedBeaconGroup struct {
	group   beaconGroup
	targets []domain.Target
}

// acquireBeaconGroup opens the runtime for one project group and acquires its
// beacon records, in isolation from every other group: one foreign project's
// conflict or store failure must not stop a beacon from landing in another
// (loto-72i — a single `loto beacon` call can now span more than one
// project's store).
//
// rg.group.isCaller picks openRuntime (which re-derives repoTop itself,
// including the errNotInGitRepo path) over openRuntimeForRepoTop, so the
// single-project call shape is unchanged byte-for-byte (AC: "Same-project
// beacon and violations behaviour is unchanged (golden diff)").
func acquireBeaconGroup(ctx context.Context, rg resolvedBeaconGroup, now time.Time, ttl time.Duration, stdout, stderr io.Writer) int {
	var rt *runtime
	var err error
	if rg.group.isCaller {
		rt, err = openRuntime(ctx)
	} else {
		rt, err = openRuntimeForRepoTop(ctx, rg.group.repoTop)
	}
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	recs := buildBeaconRecords(rg.targets, rt, now, ttl)
	// Under a subagent stamp the parent's exclusive lock is kin, not a blocker:
	// a worker locks from Bash (unstamped → parent-owned) and then writes through
	// the hook (stamped). Refusing the beacon here would leave two siblings on
	// one parent-locked file unserialized — the loto-fs84 hole, one layer up.
	kin, err := parentKin(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	acquired, err := rt.Store.AcquireLocks(rt.Ctx, recs, memoLiveProbe(rt.liveProbe()), kin...)
	if err != nil {
		return emitBeaconErr(err, stdout, stderr)
	}
	render.EmitBeaconSuccess(stdout, acquired, ttl)
	return 0
}

// emitBeaconErr maps AcquireLocks' failures onto beacon's exit codes. A
// conflict is exit 1 with the blocker rows — it means an EXCLUSIVE holder
// already owns the path, which the gate should have caught one step earlier;
// printing the holder beats printing "beacon failed".
func emitBeaconErr(err error, stdout, stderr io.Writer) int {
	var mce *store.MultiConflictError
	if errors.As(err, &mce) {
		render.EmitConflictWithTags(stdout, mce, nil)
		return 1
	}
	fmt.Fprintf(stderr, "✗ %v\n", err)
	return 3
}

// buildBeaconRecords is buildLockRecords' beacon-shaped sibling: shared mode,
// no pid, no proc-start, no branch, a supplied intent.
//
// ‡ PID stays 0 and that is the point, not an omission. The minting process is
// the hook, which exits within milliseconds of the write it is announcing. Stamp
// its pid and the very next liveness probe reads the holder as dead and reclaims
// the beacon — the leg would no-op. PID-0 is the store's existing "no durable
// liveness handle" sentinel, so the TTL is the sole authority (loto-t1tq,
// loto-j1bo), which is exactly the lease a beacon wants.
//
// SessionUUID is carried so a beacon can be told apart from a genuine peer's:
// siblings of one Claude session share a session id while holding distinct
// owner uuids, and that is the discriminator gateDecideAny uses to let a
// session's own write through its siblings' beacons (loto-xwod AC).
//
// ‡ Beacon: true is what marks the row — not the shared/pid-0 shape, which an
// ordinary `loto lock --shared` placed without LOTO_PID wears too (loto-dm4i).
// The same flag drives the store's yield rule: a beacon never overwrites a
// stronger same-owner lock the agent asked for by hand (loto-xl4g).
func buildBeaconRecords(targets []domain.Target, rt *runtime, now time.Time, ttl time.Duration) []domain.LockRecord {
	recs := make([]domain.LockRecord, 0, len(targets))
	for _, t := range targets {
		recs = append(recs, domain.LockRecord{
			Target:      t,
			OwnerUUID:   domain.AgentUUID(rt.Agent.UUID),
			SessionUUID: rt.SessionUUID,
			Intent:      beaconIntent,
			CreatedAt:   now,
			ExpiresAt:   now.Add(ttl),
			Host:        rt.Host,
			PID:         0,
			Mode:        domain.ModeShared,
			Beacon:      true,
		})
	}
	return recs
}
