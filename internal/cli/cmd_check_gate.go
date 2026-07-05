package cli

import (
	"sort"
	"time"

	"loto/internal/domain"
)

// gateKind distinguishes what denied a target: a foreign live path-prefix
// claim, or a foreign live lock/beacon (both stored as domain.LockRecord —
// a beacon is a ModeShared lock row minted by the PreToolUse hook; the gate
// treats it the same as an exclusive lock, see appendGateDenyForTarget).
const (
	gateKindClaim = "claim"
	gateKindLock  = "lock"
)

// gateDeny is one denied-path row for `loto check --gate`. BlockerPath is
// the blocker's own canonical path — a claim's PathPrefix (may be an
// ancestor of Path, not equal to it: the kuv.10 "not yet on disk" class) or
// a lock's Target.Canonical (always == Path, since locks are per-file).
type gateDeny struct {
	Path        string
	Kind        string
	HolderUUID  string
	Intent      string
	ExpiresAt   time.Time
	BlockerPath string
}

// gateDecide partitions targets into deny rows: a foreign live claim whose
// prefix overlaps a target, or a foreign live lock/beacon on a target's
// exact path. Pure — no IO, no clock read beyond ec.Now — so cmdCheck's
// gate branch (cmd_check.go) and this file's unit tests share one decision
// surface (gate-design.md component 4). Deliberately diverges from plain
// check's computeCheckConflicts (plan "gate semantics vs plain check"):
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
// Output is deterministically sorted path -> kind -> holder UUID
// (.claude/rules/design.md: same input, byte-identical output).
func gateDecide(targets []domain.Target, locks []domain.LockRecord, claims []domain.ClaimRecord, myUUID string, ec domain.EvalContext) []gateDeny {
	var rows []gateDeny
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
		return rows[i].HolderUUID < rows[j].HolderUUID
	})
	return rows
}

// appendGateDenyForTarget scans locks then claims for foreign live coverage
// of t, appending a gateDeny per distinct (kind, blocker) pair. seen dedupes
// across repeated targets in the input (a duplicate CLI arg) the same way
// computeCheckConflicts dedupes plain check's rows.
func appendGateDenyForTarget(rows []gateDeny, seen map[string]bool, t domain.Target, locks []domain.LockRecord, claims []domain.ClaimRecord, myUUID string, ec domain.EvalContext) []gateDeny {
	for i := range locks {
		l := &locks[i]
		if !domain.SameCanonical(t, l.Target) || string(l.OwnerUUID) == myUUID || ec.IsStale(*l) {
			continue
		}
		key := "lock|" + t.Canonical + "|" + l.Target.Canonical + "|" + string(l.OwnerUUID)
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, gateDeny{
			Path: t.Canonical, Kind: gateKindLock, HolderUUID: string(l.OwnerUUID),
			Intent: l.Intent, ExpiresAt: l.ExpiresAt, BlockerPath: l.Target.Canonical,
		})
	}
	for i := range claims {
		c := &claims[i]
		if !domain.PrefixOverlaps(c.PathPrefix, t.Canonical) || string(c.OwnerUUID) == myUUID || c.Expired(ec.Now) {
			continue
		}
		key := "claim|" + t.Canonical + "|" + c.PathPrefix + "|" + string(c.OwnerUUID)
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, gateDeny{
			Path: t.Canonical, Kind: gateKindClaim, HolderUUID: string(c.OwnerUUID),
			Intent: c.Intent, ExpiresAt: c.ExpiresAt, BlockerPath: c.PathPrefix,
		})
	}
	return rows
}
