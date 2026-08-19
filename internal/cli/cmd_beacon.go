package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"loto/internal/domain"
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
// identity.resolveSubagent already mints a stable per-sibling identity from the
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

	repoTop, _ := repoTopForCwd(ctx)
	// loto-z5nb: a beacon may name a path that does not exist yet — announcing
	// a Write about to CREATE it is the case a beacon exists to protect.
	targets, invalid := validateLockTargets(fs.Args(), repoTop, true)
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

	now := time.Now()
	recs := buildBeaconRecords(targets, rt, now, *ttl)
	acquired, err := rt.Store.AcquireLocks(rt.Ctx, recs, memoLiveProbe(rt.liveProbe()))
	if err != nil {
		return emitBeaconErr(err, stdout, stderr)
	}
	render.EmitBeaconSuccess(stdout, acquired, *ttl)
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
// owner uuids, and that is the discriminator `loto guard` uses to let a session
// move its own tree (loto-xwod AC).
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
