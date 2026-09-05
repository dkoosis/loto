package domain

import "time"

// AgentUUID is the stable per-agent identity that owns locks and tags. It is a
// distinct named type (not a bare string) so that transposing it with an
// adjacent untyped string argument — e.g. a session UUID, a target path, an
// intent — is a compile error rather than a silent runtime bug (loto-inf4,
// stage 1 of loto-34n3). Values cross untyped edges (env vars, CLI flags,
// sqlite columns) via explicit AgentUUID(s)/string(x) conversions. The
// identity package cannot reference this type — the arch layering pins
// internal/identity → ∅ — so an identity-sourced UUID is converted to
// AgentUUID at the cli boundary.
type AgentUUID string

// SessionUUID is the per-session identity (one Claude session = one id, shared
// by every shell-out from that session; sourced from LOTO_SESSION_ID). It is a
// distinct named type from AgentUUID so the runtime.SessionUUID-vs-Agent.UUID
// overload — the original swap hazard — and ReleaseBySession(agent, session)
// can no longer transpose silently (loto-ww4x, stage 2 of loto-34n3). Values
// cross untyped edges (env var, sqlite session_uuid column) via explicit
// SessionUUID(s)/string(x) conversions.
type SessionUUID string

// Canonical is a canonicalized, repo-relative target path — the cleaned form
// minted by Canonicalize and carried in Target.Canonical. It is the path member
// of the swap-safe identity family (loto-ip6c, stage 3 of loto-34n3): a distinct
// named type so a path can't silently transpose with an adjacent bare-string
// field — e.g. NewTag{TargetCanonical, TaggerUUID} can no longer be filled in the
// wrong order and still compile. Scoped to the tag surface (Tag/NewTag plus
// ListAliveForTarget/ListAliveByTargets) where the path sits beside other
// strings; Target.Canonical stays a bare string (the persisted-form home and an
// untyped edge), so values cross via explicit Canonical(s)/string(x) conversions
// at that boundary, the sqlite target_canonical column, and CLI callers.
type Canonical string

type LockRecord struct {
	Target      Target
	OwnerUUID   AgentUUID
	SessionUUID SessionUUID
	Intent      string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Host        string
	PID         int
	// ProcStart is the holder process's OS start-time, read at acquire on the
	// local host. It defeats PID reuse: if the original holder dies and the OS
	// recycles its PID to an unrelated process, the recycled occupant's
	// start-time won't match, so the lock is correctly treated as stale
	// (loto-kwlp). The encoding is opaque and per-OS — it is only ever compared
	// for equality on the same host/OS (IsStale probes liveness on
	// l.Host == thisHost only). Zero means UNKNOWN: legacy rows predating this
	// field, or OSes where start-time can't be read. Unknown falls back to
	// pid-alive-only (today's behavior).
	ProcStart int64
	Branch    string
	// Mode is the lease mode: ModeShared (multi-reader, advisory only, write-bit
	// NOT stripped) or ModeExclusive (sole-writer, write-bit stripped). Empty
	// string reads as exclusive — preserves the pre-mode binary-lock semantics
	// for legacy rows (loto-k5el.2). Normalize via EffectiveMode().
	Mode string
	// Beacon marks a row the PreToolUse gate minted on a writing agent's
	// behalf, as opposed to a lease an agent asked for. Persisted (locks.beacon)
	// rather than inferred, because the row shape does not separate the two —
	// see IsBeacon.
	Beacon bool
	// Epoch is the generation counter of the AUTHORIZATION to write this path,
	// not of the row (git-gate.md "Ownership epoch semantics", loto-ovno.2).
	// Renew/heartbeat (RefreshLocks, or a same-owner re-acquire of a still-live
	// row) preserves it — the holder never invalidated its own outstanding
	// candidates by proving it is still alive. Release+reacquire, transfer to a
	// new owner, a stale-owner reclaim, and a force-break each increment it —
	// every one of those is, mechanically, a fresh INSERT rather than an UPDATE
	// of the existing row (the prior row is gone by the time this one lands),
	// which is the single condition the store increments on. A candidate
	// envelope pins the epoch it captured per path; admission (a later bead)
	// compares that against the CURRENT epoch here to tell "the same
	// uninterrupted authorization" from "someone else has held this since."
	Epoch int64
}

const (
	ModeShared    = "shared"
	ModeExclusive = "exclusive"
)

// EffectiveMode normalizes a possibly-empty Mode to exclusive (legacy default).
func (l LockRecord) EffectiveMode() string {
	if l.Mode == ModeShared {
		return ModeShared
	}
	return ModeExclusive // empty or any non-"shared" value → exclusive
}

// IsBeacon reports whether this row was minted by the PreToolUse gate on a
// writing agent's behalf rather than taken by an agent that asked for it
// (loto-xwod). It reads the persisted flag and nothing else.
//
// The distinction is not cosmetic. `loto guard` lets a session move its own
// tree past its OWN siblings' beacons — they only say "an agent of mine is
// writing here" — while a sibling's real exclusive lock still refuses the move.
// A checkout under declared, uncommitted territory is precisely what destroyed
// an agent's work on 2026-08-14.
//
// ‡ The flag is persisted because the shape cannot carry it (loto-dm4i, Codex
// #249). A beacon is shared with PID 0 — shared so two siblings' beacons never
// collide at acquire, pid-less because the minting hook exits milliseconds
// after the write it announces. But an ordinary `loto lock --shared` placed
// without LOTO_PID stores exactly that shape too: PID 0 is the documented
// "no durable liveness handle" sentinel, not a beacon marker. Reading the
// shape therefore classified a real shared lease as a beacon, and guard's
// same-session carve-out then waived it and moved the tree — the failure the
// carve-out was written to avoid, pointed the other way.
func (l LockRecord) IsBeacon() bool {
	return l.Beacon
}

// ClaimRecord is a coarse path-prefix territory reservation: "this package is
// mine this session", distinct from the per-file, per-edit LockRecord
// (loto-7af9). Claims are TTL-only leases — no PID/liveness fields, no mode;
// Expired is the sole staleness authority. PathPrefix is a canonical
// repo-relative directory prefix minted by CanonicalizePrefix.
type ClaimRecord struct {
	PathPrefix  string
	OwnerUUID   AgentUUID
	SessionUUID SessionUUID
	Intent      string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Host        string
}

// Expired reports whether the claim's TTL lease has lapsed at now. Claims
// strip no write bits and carry no PID, so unlike lock staleness there is no
// liveness refinement — the lease boundary is the whole story.
func (c ClaimRecord) Expired(now time.Time) bool {
	return !now.Before(c.ExpiresAt)
}

// CandidateClaim is a durable, per-path territory hold on behalf of a
// candidate awaiting promotion (git-gate.md "Claim lifecycle", loto-ovno.2).
// It is minted when admission (a later bead) converts an accepted candidate's
// live session lease into this — and it MUST survive that conversion, unlike a
// LockRecord: the admitting/promoting process can exit while the candidate is
// still pending review, and the claim has to keep blocking overlapping
// acquisitions until the candidate is promoted, rejected, or withdrawn.
//
// One row per (PathCanonical, CandidateID) — a candidate's write-set claims
// every path it touches, mirroring LockRecord's per-file granularity (never a
// prefix; a candidate's write-set is exact files from lane.Commit).
//
// Deliberately carries no TTL. A candidate under review has no natural
// deadline — review takes as long as it takes — so unlike LockRecord (TTL +
// liveness) or ClaimRecord (TTL only), this record's SOLE staleness authority
// is CandidateClaimIsDead: was the process that minted it provably killed
// without ever resolving it. See PID/ProcStart below.
type CandidateClaim struct {
	PathCanonical string
	CandidateID   string
	OwnerUUID     AgentUUID
	SessionUUID   SessionUUID
	CreatedAt     time.Time
	Host          string
	// PID/ProcStart identify the process that minted (or most recently
	// touched) this claim — NOT necessarily the original proposer's editing
	// session. "claim liveness for the promotion claim uses PID+proc-start
	// reclaim" (git-gate.md): if that process is provably dead with the claim
	// still unresolved, the claim is abandoned territory, reclaimable the same
	// way a crashed lock holder's row is.
	PID       int
	ProcStart int64
}

// CandidateClaimIsDead reports whether the process that minted cc is provably
// gone — the sole staleness authority for a durable candidate claim (no TTL;
// see CandidateClaim's doc). Lifted into LockRecord shape to reuse the exact
// probe every other liveness verdict in this store consults, rather than
// growing a third "is this holder alive" definition (ClaimIsStale already
// makes this argument for TTL-bearing claims; the same reasoning applies here
// one level further — no TTL branch at all, since none exists to lapse). A nil
// probe or an UNKNOWN verdict both answer false: without a positive DEAD
// verdict, a claim is never reclaimed out from under a candidate still under
// review.
func (c EvalContext) CandidateClaimIsDead(cc CandidateClaim) bool {
	if c.Live == nil {
		return false
	}
	return c.Live(LockRecord{
		OwnerUUID:   cc.OwnerUUID,
		SessionUUID: cc.SessionUUID,
		Host:        cc.Host,
		PID:         cc.PID,
		ProcStart:   cc.ProcStart,
	}) == LivenessDead
}

// CandidateClaimReclaimGrace bounds how young a zero-evidence candidate claim
// (PID<=0, probe UNKNOWN — see CandidateClaimIsAbandoned) must be before
// doctor's sweep will reclaim it. Set to the same 30-minute default the lock
// layer already uses for its own TTL-only degraded mode (cmd_lock.go's and
// cmd_refresh.go's `-ttl` default) — domain cannot import cli (arch layering:
// domain → ∅), so the VALUE is restated here rather than imported; this is
// the same policy number, not a new one.
//
// Without this floor, a claim minted seconds ago by a degraded-mode submitter
// (no durable LOTO_PID — documented as the expected, silent case for a bare
// shell/cron) or from another host (the probe reads UNKNOWN on host mismatch
// before it ever inspects PID or session) would be reclaimed by a peer's very
// next `loto doctor --repair` while genuinely still under review — violating
// CandidateClaim's own "no natural deadline... reclaimable [only when]
// provably killed" contract (PR #298 review). A provably DEAD claim (the
// switch's other reclaiming branch) is exempt from this floor and reclaims at
// any age — there the process is confirmed gone, not merely unread.
const CandidateClaimReclaimGrace = 30 * time.Minute

// CandidateClaimIsAbandoned is doctor's stale-candidate-claim reclaim
// authority (loto-u2p7) — deliberately BROADER than CandidateClaimIsDead
// above, and scoped to doctor's sweep alone: every other caller (admission's
// authorizedPaths, promotion's re-validation) keeps the conservative
// Dead-only predicate.
//
// A provably-DEAD claim is abandoned at any age, same as CandidateClaimIsDead.
// In addition, a claim minted with NO durable pid (PID<=0, the same
// "no-durable-liveness-handle" sentinel IsBeacon/pidVerdict document
// elsewhere) whose probe comes back UNKNOWN — no session record, no socket,
// nothing to read — is ALSO abandoned, but only once it is at least
// CandidateClaimReclaimGrace old: it carries zero evidence either way, and
// unlike a LockRecord or a ClaimRecord it has no TTL backstop to eventually
// self-heal on (CandidateClaim's own doc), so leaving the zero-evidence case
// entirely un-reclaimed is the bug loto-u2p7 reports — doctor reporting
// healthy while a dead session's claim blocks `loto lock` on its paths
// forever. The grace floor is what keeps a claim that is merely YOUNG (not
// dead) from reading the same as one that is abandoned.
//
// A claim with a durable pid (PID>0) that merely reads UNKNOWN — a foreign
// host, say — is NOT reclaimed here at any age: it still has a witness (the
// pid) this vantage point just can't read, and the risk asymmetry that keeps
// CandidateClaimIsDead conservative applies to it in full. Only the
// zero-evidence, no-TTL-backstop combination gets the broader (grace-gated)
// treatment.
func (c EvalContext) CandidateClaimIsAbandoned(cc CandidateClaim) bool {
	if c.Live == nil {
		return false
	}
	switch c.Live(LockRecord{
		OwnerUUID:   cc.OwnerUUID,
		SessionUUID: cc.SessionUUID,
		Host:        cc.Host,
		PID:         cc.PID,
		ProcStart:   cc.ProcStart,
	}) {
	case LivenessDead:
		return true
	case LivenessUnknown:
		return cc.PID <= 0 && !c.Now.Before(cc.CreatedAt.Add(CandidateClaimReclaimGrace))
	case LivenessAlive:
		return false
	default:
		return false
	}
}

// Event is an append-only audit row. SubjectUUID is the affected agent (for
// lock_broken / lock_reclaimed_stale); empty otherwise.
type Event struct {
	ID          string
	Target      Target
	Kind        string
	ActorUUID   string
	SubjectUUID string
	Reason      string
	CreatedAt   time.Time
}
