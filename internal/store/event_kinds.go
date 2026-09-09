package store

import "strings"

// Event kind constants — the single declaration site for every value ever
// written to events.event_kind (loto-123y). They used to be split across
// locks.go and gate_stats.go, with the schema's CHECK list and both events
// rebuild DDLs hand-typing the same strings a second and third time; adding a
// kind meant remembering all five surfaces, and a miss was caught only at
// runtime by a CHECK-constraint failure.
//
// Adding a kind now means one edit here: append the constant and add it to
// allEventKinds below. schema.sql's CHECK clause (via the
// eventKindCheckPlaceholder substitution in schemaSQL) and
// ensureEventsCheckAllKinds's rebuild DDL both render from allEventKinds,
// so neither needs a second hand-typed copy of the list.
//
// ensureEventsCheckCurrent in store.go is the one deliberate exception: its
// rebuild DDL is a frozen historical snapshot of the CHECK list as it stood
// before staged_lock_gate_fired (and events.detail) existed. It must NOT be
// re-derived from allEventKinds — see its own doc comment for why.
const (
	EventLockAcquired       = "lock_acquired"
	EventLockReleased       = "lock_released"
	EventLockBroken         = "lock_broken"
	EventLockReclaimedStale = "lock_reclaimed_stale"
	// EventModeRestoreFailed is emitted only by doctor's chmod-era migration
	// now (loto-zssw); EventAcquireRollbackStart is emitted by nothing at all.
	// Both stay declared: the schema's event_kind CHECK still names them, and
	// rows written before the strip retired are still readable.
	EventModeRestoreFailed    = "mode_restore_failed"
	EventAcquireRollbackStart = "acquire_rollback_started"
	EventLockDowngraded       = "lock_downgraded"
	EventLockRefreshed        = "lock_refreshed"
	// EventGateBypass is emitted every time LOTO_GATE=off bypasses admission
	// (loto-ovno.4, git-gate.md "The gate can never become the outage"). No
	// Target — a bypass is a session-scoped fact, not a per-path one — so
	// TargetCanonical is written empty. ActorUUID names who bypassed.
	EventGateBypass = "gate_bypass"
	// Admission verdict event kinds (loto-ovno.9). Every candidate that
	// reaches a verdict leaves exactly one of these, so the rejection
	// taxonomy stops being a thing the CLI prints once and becomes a thing
	// the repo can count.
	//
	// ‡ The rejection CLASS rides in Event.Reason, not in the kind: a kind
	// per class would need a CHECK-constraint migration every time the
	// taxonomy grows, and would make "how many candidates were rejected at
	// all" a query over a list someone has to remember to extend.
	EventCandidateAccepted = "candidate_accepted"
	EventCandidateRejected = "candidate_rejected"
	// EventStagedGateFired is the firing counter behind `loto check --held`
	// (loto-7oik). One row per firing — not per path — so counting rows of
	// this kind answers "how often would this gate have refused a commit",
	// which is the evidence the advisory-first rollout is gathering before
	// the gate's default is reconsidered. No Target, same reason
	// EventGateBypass has none: a firing is a commit-scoped fact, and the
	// per-path detail rides in Detail as JSON. ActorUUID names the committer.
	EventStagedGateFired = "staged_lock_gate_fired"
	// I1's two counters (loto-ea8y.4, enforcement-design §10b row 1). They
	// answer one question — "is the three-shape denylist refusing the right
	// things?" — and the answer is a RATIO, which is why both kinds exist
	// rather than refusals alone.
	//
	// EventRefRefused is written by `loto hook ref` when the
	// reference-transaction hook aborts a protected ref shape. Target is the
	// ref name; ActorUUID the refused owner; Reason the shape
	// (head-symref | stash | branch-delete); Detail the JSON (old, new, ref,
	// live) §10b names.
	//
	// EventRefRefusedOverridden is written by `loto claim .` when the same
	// owner takes the checkout-wide claim within refOverrideWindow of a
	// refusal — the operator saying that refusal was wrong. Overrides
	// approaching refusals is the signal to revisit `allow` itself, not to
	// press the operator harder.
	EventRefRefused           = "ref_refused"
	EventRefRefusedOverridden = "ref_refused_overridden"
	// EventHookTiming is what the tree hook costs: one row per pre and one per
	// post (enforcement-design.md §10b row 3). Reason says which half wrote it
	// ("pre" or "post"); the payload rides in Detail as JSON — call_id,
	// pre_ms or post_ms, status_paths, locked_bytes.
	//
	// ‡ The counter exists because a decision reads it, which is §10b's whole
	// rule for writing one: p99 pre + post over 250 ms on this machine, or p50
	// over 50 ms in any repo, and the digest-on-every-call design is measured
	// out rather than argued out. No Target — a call is not a path — so
	// TargetCanonical is written empty, same as EventGateBypass.
	EventHookTiming = "hook_timing"
)

// allEventKinds is every event kind currently admitted by events.event_kind's
// CHECK constraint, in the order schema.sql and the current (newest) rebuild
// DDL render them. Order is cosmetic — SQL's IN-list doesn't care — held
// stable only so generated DDL doesn't churn for no reason.
var allEventKinds = []string{
	EventLockAcquired,
	EventLockReleased,
	EventLockBroken,
	EventLockReclaimedStale,
	EventModeRestoreFailed,
	EventAcquireRollbackStart,
	EventLockDowngraded,
	EventLockRefreshed,
	EventGateBypass,
	EventCandidateAccepted,
	EventCandidateRejected,
	EventStagedGateFired,
	EventRefRefused,
	EventRefRefusedOverridden,
	EventHookTiming,
}

// eventKindCheckSQL renders allEventKinds as the comma-joined, single-quoted
// list a `CHECK (event_kind IN (...))` clause needs. The one place that turns
// the Go declaration into SQL text — schema.sql and the current rebuild DDL
// both call this rather than hand-typing the list a second and third time.
func eventKindCheckSQL() string {
	quoted := make([]string, len(allEventKinds))
	for i, k := range allEventKinds {
		quoted[i] = "'" + k + "'"
	}
	return strings.Join(quoted, ",")
}
