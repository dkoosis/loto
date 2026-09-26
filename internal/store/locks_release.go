package store

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"loto/internal/domain"
)

// ReleaseLocks releases each target best-effort under the project op-flock in
// a single transaction (SELECT … WHERE IN, batched DELETE). Returns one
// ReleaseResult per input target in input order — render owns the canonical
// sort for stable output. The returned error is non-nil only on internal/SQL
// failures; per-target outcomes (no-lock, not-owner, reclaimed-stale,
// restore-failed) are reported via ReleaseResult.State.
//
// Stale-aware (loto-ebkc): when the caller holds no row at a target and EVERY
// foreign holder is stale under live, the plain unlock reclaims them all —
// delete + lock_reclaimed_stale audit in the same tx — instead of bouncing to
// not-owner. One live foreign holder vetoes the whole target
// (authorizeHolders, loto-w77f parity). The caller's own row always wins
// first (prefer-own, loto-k5el.2) and never piggybacks a co-holder reclaim.
func (s *Store) ReleaseLocks(ctx context.Context, targets []domain.Target, agent domain.AgentUUID, live domain.HolderLiveProbe) ([]ReleaseResult, error) {
	byAgent := string(agent) // internal store helpers thread the owner as a plain string
	if len(targets) == 0 {
		return []ReleaseResult{}, nil
	}

	var results []ReleaseResult
	err := s.withLockBatchTx(ctx, targets, live, func(tx *sql.Tx, existing map[string][]domain.LockRecord, ec domain.EvalContext, now time.Time) error {
		var owned []string
		var reclaims []domain.LockRecord
		results, owned, reclaims = classifyReleases(targets, existing, byAgent, ec)
		return s.applyReleaseChangesTx(ctx, tx, owned, reclaims, byAgent, now)
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// applyReleaseChangesTx writes both release branches into the caller's tx:
// the batched own-row releases (ack + delete + lock_released events) and the
// per-holder stale reclaims (lock_reclaimed_stale + delete via reclaimStaleTx).
// Reclaims orphan the dead holders' pending tags (a dead peer never "reads"
// its notes) — those are GC'd per holder via gcTagsForReclaimTx (loto-qg0r
// intent, targeted form). Event rotation fires when EITHER branch appended
// (mirrors AcquireLocks→rotateEventsTx): a release/reclaim-heavy workload that
// rarely acquires would otherwise grow the events table unbounded (loto-bvdk).
func (s *Store) applyReleaseChangesTx(ctx context.Context, tx *sql.Tx, owned []string, reclaims []domain.LockRecord, byAgent string, now time.Time) error {
	if len(owned) > 0 {
		if err := s.applyOwnedReleasesTx(ctx, tx, owned, byAgent, now); err != nil {
			return err
		}
	}
	for i := range reclaims {
		if err := reclaimStaleTx(ctx, tx, reclaims[i], byAgent, now); err != nil {
			return err
		}
		if err := gcTagsForReclaimTx(ctx, tx, reclaims[i]); err != nil {
			return err
		}
	}
	if len(owned) > 0 || len(reclaims) > 0 {
		if err := rotateEventsTx(ctx, tx, now); err != nil {
			return err
		}
	}
	return nil
}

// gcTagsForReclaimTx deletes exactly the reclaimed holder's tag rows. The
// blanket gcTagsTx is WRONG in this tx: its orphan clause deletes any tag
// whose host lock is gone, and in a mixed batch the own-release branch has
// just acked its tags AND deleted its host-lock row a few statements earlier
// — the blanket pass would eat those freshly-acked audit rows before the
// retention window ever saw them (review P2). Keying on the reclaimed row's
// (target, owner, created_at) identity hits only the dead holder's tags.
func gcTagsForReclaimTx(ctx context.Context, tx *sql.Tx, stale domain.LockRecord) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM tags WHERE target_canonical = ? AND lock_owner_uuid = ? AND lock_created_at = ?`,
		stale.Target.Canonical, string(stale.OwnerUUID), stale.CreatedAt.UnixNano())
	return err
}

// applyOwnedReleasesTx acks tags, deletes the owned host-lock rows, and emits
// the lock_released events — all in the caller's tx so the row deletes and
// audit stay atomic. Event rotation is hoisted to the caller, which trims once
// for both the own-release and reclaim branches.
func (s *Store) applyOwnedReleasesTx(ctx context.Context, tx *sql.Tx, owned []string, byAgent string, now time.Time) error {
	// Ack tags BEFORE deleting the host locks: the host-lock match must still
	// resolve to set acked_at; if we DELETE first the tags would orphan instead,
	// losing the audit ack (edge #6 distinguishes release-ack from break-orphan).
	//
	// ‡ worktree-scoped (loto-8z87): s.repoTop names THIS store's own checkout.
	// existing (loaded by withLockBatchTx) is already scoped to it via
	// scopeToWorktree, so `owned` never names a foreign-worktree canonical —
	// but with the widened PK, one owner can hold a SIBLING row at the same
	// canonical from another worktree, and the raw DELETE/UPDATE below must not
	// reach past its own worktree to find it.
	if err := ackTagsForReleaseTx(ctx, tx, s.keys(), owned, byAgent, s.repoTop); err != nil {
		return err
	}
	if err := deleteOwnedTx(ctx, tx, s.keys(), owned, byAgent, s.repoTop); err != nil {
		return err
	}
	// Emit lock_released events in the same tx (atomic with the row deletes).
	evs := make([]domain.Event, len(owned))
	for i, canonical := range owned {
		evs[i] = domain.Event{
			Target:    domain.Target{Canonical: canonical},
			Kind:      EventLockReleased,
			ActorUUID: byAgent,
			CreatedAt: now,
		}
	}
	return appendEventsTx(ctx, tx, evs)
}

// classifyReleases walks input targets in order, classifying each against its
// full holder set: own row → release it (prefer-own; co-holders untouched) ·
// no rows → no-lock · all foreign holders stale → reclaim every holder ·
// ≥1 live foreign holder → not-owner (live-holder veto, loto-w77f). Returns
// the per-target results, the canonicals whose own row gets the batched
// DELETE, and the stale foreign rows to reclaim individually.
func classifyReleases(targets []domain.Target, existing map[string][]domain.LockRecord, byAgent string, ec domain.EvalContext) ([]ReleaseResult, []string, []domain.LockRecord) {
	results := make([]ReleaseResult, len(targets))
	owned := make([]string, 0, len(targets))
	var reclaims []domain.LockRecord
	firstIdx := make(map[string]int, len(targets))
	for i, t := range targets {
		results[i].Target = t
		if j, dup := firstIdx[t.Canonical]; dup {
			// Duplicate input target (`unlock a.go a.go`): never re-append to
			// owned/reclaims — that doubled the lock_released /
			// lock_reclaimed_stale audit (review P3). When the first occurrence
			// consumed the row(s) the duplicate is no-lock; when it didn't
			// (not-owner veto, no-lock) the duplicate mirrors it — a still-held
			// lock must not be reported as gone (review P2, #212).
			switch results[j].State { //nolint:exhaustive // default mirrors every non-consuming outcome
			case StateUnlocked, StateReclaimedStale:
				results[i].State = StateNoLock
			default:
				results[i].State = results[j].State
				results[i].Owner = results[j].Owner
				results[i].Mode = results[j].Mode
			}
			continue
		}
		firstIdx[t.Canonical] = i
		holders := existing[t.Canonical]
		switch own := ownHolder(holders, byAgent); {
		case len(holders) == 0:
			results[i].State = StateNoLock
		case own != nil:
			results[i].State = StateUnlocked
			results[i].Mode = own.Mode
			owned = append(owned, t.Canonical)
		case authorizeHolders(holders, ec, false) == nil:
			// The stale-reclaim gate is authorizeHolders — the same single-
			// sourced live-holder veto BreakStale uses (loto-w77f), not a
			// re-derived predicate. All holders share one mode (all-shared-or-
			// one-exclusive invariant), so holders[0] drives restore; holders
			// arrive created_at,owner-ordered, so Owner is deterministic.
			results[i].State = StateReclaimedStale
			results[i].Mode = holders[0].Mode
			results[i].Owner = string(holders[0].OwnerUUID)
			reclaims = append(reclaims, holders...)
		default:
			results[i].State = StateNotOwner
			results[i].Owner = vetoingHolder(holders, ec)
		}
	}
	return results, owned, reclaims
}

// ownHolder returns the caller's row among a target's holders, or nil.
func ownHolder(holders []domain.LockRecord, byAgent string) *domain.LockRecord {
	for i := range holders {
		if string(holders[i].OwnerUUID) == byAgent {
			return &holders[i]
		}
	}
	return nil
}

// vetoingHolder names the first LIVE holder in created_at,owner order — the
// one whose liveness vetoed the stale-reclaim — so the not-owner report points
// at an owner that actually blocks, not an arbitrary (possibly dead) one.
func vetoingHolder(holders []domain.LockRecord, ec domain.EvalContext) string {
	for i := range holders {
		if ec.AuthorizeBreak(holders[i], false) != nil {
			return string(holders[i].OwnerUUID)
		}
	}
	return string(holders[0].OwnerUUID) // unreachable when a veto occurred; deterministic fallback
}

// ackTagsForReleaseTx marks every pending tag whose host lock is in the
// release set as acked. Run inside the release tx BEFORE deleteOwnedTx so the
// host-lock subquery still matches; running it after would silently orphan
// tags instead of acking them (would still get GC'd by doctor, but the audit
// would lose the explicit ack timestamp).
//
// worktree scopes the host-lock subquery to the caller's own checkout, via
// worktreeFilter (loto-8z87). Under the widened PK a same-owner SIBLING row
// from another worktree can share this exact (target, owner) pair; without
// the scope its created_at could satisfy the tuple match and ack a tag that
// belongs to the sibling's still-live lock.
func ackTagsForReleaseTx(ctx context.Context, tx *sql.Tx, k keyMatch, canonicals []string, byAgent, worktree string) error {
	placeholders, args := k.inStrings(canonicals)
	wtCond, wtArgs := worktreeFilter(worktree)
	args = append(append([]any{time.Now().UnixNano()}, args...), byAgent)
	args = append(args, wtArgs...)
	// ‡ Every leg keys under k, the tuple's target member included: a tag this
	// binary minted carries the folded key while its host lock row may carry an
	// older loto's on-disk spelling, so a byte-exact tuple would orphan the tag
	// instead of acking it and lose the release half of the audit (loto-8soe).
	_, err := tx.ExecContext(ctx, `UPDATE tags SET acked_at = ?`+ //nolint:gosec // G202 placeholders are '?' chars only, all data via args
		` WHERE acked_at IS NULL`+
		` AND (`+k.col("target_canonical")+`, lock_owner_uuid, lock_created_at) IN (`+
		`   SELECT `+k.col("target_canonical")+`, owner_uuid, created_at FROM locks`+
		`   WHERE `+k.col("target_canonical")+` IN (`+placeholders+`) AND owner_uuid = ? AND `+wtCond+
		` )`, args...)
	return err
}

// deleteOwnedTx removes `locks` rows for the given canonical paths owned by
// byAgent in one statement. k is what lets it reach a row an older loto wrote
// in the on-disk spelling — without it `loto unlock foo.go` deleted nothing
// and still reported success (loto-8soe).
//
// worktree scopes the delete to the caller's own checkout, via worktreeFilter
// (loto-8z87). Under the widened PK (target_canonical, owner_uuid, worktree)
// the same owner can hold a SIBLING row on the same canonical from another
// linked worktree; a bare owner+canonical DELETE would take both, undoing the
// PK widening's whole point the moment anything actually released.
func deleteOwnedTx(ctx context.Context, tx *sql.Tx, k keyMatch, canonicals []string, byAgent, worktree string) error {
	placeholders, args := k.inStrings(canonicals)
	wtCond, wtArgs := worktreeFilter(worktree)
	args = append(args, byAgent)
	args = append(args, wtArgs...)
	_, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE `+k.col("target_canonical")+` IN (`+placeholders+`) AND owner_uuid = ? AND `+wtCond, args...) //nolint:gosec // G202 placeholders are '?' chars only, all data via args

	return err
}

// ReleaseBySession atomically releases all locks owned by byAgent in the given
// session. If sessionUUID is empty, it releases all locks owned by byAgent
// regardless of session — the agent-scoped fallback for direct CLI use where
// no LOTO_SESSION_ID is pinned. This is the atomic replacement for the
// list+filter+release dance in unlockAll: a single SQL query finds matching
// rows and deletes them in one transaction, closing the TOCTOU gap where the
// old path could miss locks created between ListLocks and ReleaseLocks.
//
// It also releases the agent's claims in the SAME transaction, returning the
// released claim prefixes alongside the lock results: a session-end unlock must
// clear claimed territory too, or a crashed/ended agent's claim squats until TTL
// (loto-ei5). Claims strip no write bits and carry no PID, so unlike locks they
// need no post-commit filesystem restore and fold safely into this tx — making
// the lock+claim release atomic (Codex #219 P1: a separate claim tx could leave
// claims squatting after the lock tx already committed).
//
// callerWorktree is the absolute toplevel of the caller's own worktree. When a
// matching lock OR claim row was stamped from a different, known worktree
// (LockRecord.Worktree / ClaimRecord.Worktree, loto-3eq6), the
// sweep refuses entirely — nothing is released — and reports the rows via
// SessionRelease.Ambiguous (loto-19bz): owner and session ids can both
// collapse onto one value across sibling subagents sharing a parent Claude
// Code session's env, and a plain `unlock --all` from one sibling must not
// read a peer's lock or claim as its own just because the shared ids matched.
// The worktree, not the branch, is the discriminator (Codex #371): a branch
// switch inside one worktree does not make its own locks foreign, and two
// worktrees can share a branch name or a detached commit. Pass "" to skip the
// check (git failed, or the caller has none to compare).
func (s *Store) ReleaseBySession(ctx context.Context, agent domain.AgentUUID, sessionUUID domain.SessionUUID, onlyIntent, callerWorktree string) (SessionRelease, error) {
	byAgent := string(agent) // internal store helpers thread the owner as a plain string
	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return SessionRelease{}, err
	}
	defer flock.release()

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return SessionRelease{}, err
	}
	defer cleanup()

	// Find all targets matching agent (+session if pinned, +intent if filtered).
	canonicals, err := loadSessionTargetsTx(ctx, tx, byAgent, string(sessionUUID), onlyIntent)
	if err != nil {
		return SessionRelease{}, err
	}
	// Ambiguity is judged over BOTH row kinds before anything is deleted: a
	// sibling holding only a claim must refuse the sweep as surely as one
	// holding a lock (Codex #371 P1). Nothing in this sweep is released when
	// even one row cannot be told apart from a peer's.
	canonicals, foreign, err := sessionAmbiguityTx(ctx, tx, canonicals, byAgent, string(sessionUUID), onlyIntent, callerWorktree, time.Now())
	if err != nil {
		return SessionRelease{}, err
	}
	if len(foreign) > 0 {
		return SessionRelease{Ambiguous: foreign}, nil
	}

	// Claims: pure DB, nothing to restore after commit.
	//
	// ‡ Skipped under an intent filter (loto-lzap). A claim records territory a
	// lane reserved, not the write-set it took, and carries no intent to match —
	// sweeping claims on a filtered release would drop the one thing the filter
	// exists to leave standing.
	var claimPrefixes []string
	if onlyIntent == "" {
		claimPrefixes, err = deleteClaimsBySessionTx(ctx, tx, byAgent, string(sessionUUID))
		if err != nil {
			return SessionRelease{}, err
		}
	}
	if len(canonicals) == 0 {
		// No locks — still commit any claim deletes above.
		if err := tx.Commit(); err != nil {
			return SessionRelease{}, err
		}
		return SessionRelease{Results: []ReleaseResult{}, ClaimPrefixes: claimPrefixes}, nil
	}
	if err := s.releaseSessionTargetsTx(ctx, tx, canonicals, byAgent, callerWorktree); err != nil {
		return SessionRelease{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionRelease{}, err
	}

	results := make([]ReleaseResult, len(canonicals))
	for i, c := range canonicals {
		results[i] = ReleaseResult{
			Target: domain.Target{Canonical: c.Canonical},
			State:  StateUnlocked,
			Mode:   c.Mode,
		}
	}
	flock.release()
	return SessionRelease{Results: results, ClaimPrefixes: claimPrefixes, Intents: distinctIntents(canonicals)}, nil
}

// releaseSessionTargetsTx acks tags, deletes the agent's host locks and emits
// their release events for the matched rows, inside ReleaseBySession's tx.
// callerWorktree scopes the delete the same way ReleaseLocks scopes on
// s.repoTop (loto-8z87): sessionAmbiguityTx already refused the whole sweep
// when a canonical carried a foreign, LIVE row, but `canonicals` can still
// carry a same-owner row from another worktree when callerWorktree == "" (git
// lookup failed) or the foreign row was already stale — the raw DELETE below
// must still not reach past its own worktree for it.
func (s *Store) releaseSessionTargetsTx(ctx context.Context, tx *sql.Tx, canonicals []sessionTarget, byAgent, callerWorktree string) error {
	paths := make([]string, len(canonicals))
	for i, c := range canonicals {
		paths[i] = c.Canonical
	}
	// Ack tags before deleting host locks (same ordering as ReleaseLocks).
	if err := ackTagsForReleaseTx(ctx, tx, s.keys(), paths, byAgent, callerWorktree); err != nil {
		return err
	}
	if err := deleteOwnedTx(ctx, tx, s.keys(), paths, byAgent, callerWorktree); err != nil {
		return err
	}
	return emitLockReleaseEventsTx(ctx, tx, canonicals, byAgent, time.Now())
}

// SessionRelease is what one ReleaseBySession transaction actually did.
//
// ‡ Intents is the reason this is a struct rather than two return values
// (loto-lzap). A caller that wants to warn "this sweep spanned more than one
// lane's intent" used to read the intents from a ListLocks BEFORE the release,
// which could observe one intent while the release deleted two — announcing
// nothing about the very peer lock it took. Reporting the intents of the rows
// the transaction deleted is the only version that cannot lie.
type SessionRelease struct {
	Results       []ReleaseResult
	ClaimPrefixes []string
	// Intents are the distinct intents of the released rows, sorted. Empty
	// when nothing was released.
	Intents []string
	// Ambiguous names the lock and claim rows the sweep found stamped from a
	// worktree other than the caller's own (loto-19bz). Non-empty means the WHOLE sweep was refused —
	// Results, ClaimPrefixes and Intents are all empty, and nothing changed in
	// the store, whatever else this call might otherwise have released.
	Ambiguous []AmbiguousHold
}

// distinctIntents collects the sorted, deduplicated intents of the rows a
// release actually deleted.
func distinctIntents(canonicals []sessionTarget) []string {
	seen := make(map[string]bool, len(canonicals))
	var out []string
	for _, c := range canonicals {
		if !seen[c.Intent] {
			seen[c.Intent] = true
			out = append(out, c.Intent)
		}
	}
	sort.Strings(out)
	return out
}

// emitLockReleaseEventsTx appends one lock_released event per released target
// and trims the events table, both in tx (atomic with the row deletes). The
// rotate mirrors AcquireLocks→rotateEventsTx so a release-heavy workload can't
// grow the events table unbounded (loto-bvdk).
func emitLockReleaseEventsTx(ctx context.Context, tx *sql.Tx, canonicals []sessionTarget, byAgent string, now time.Time) error {
	evs := make([]domain.Event, len(canonicals))
	for i, c := range canonicals {
		evs[i] = domain.Event{
			Target:    domain.Target{Canonical: c.Canonical},
			Kind:      EventLockReleased,
			ActorUUID: byAgent,
			CreatedAt: now,
		}
	}
	if err := appendEventsTx(ctx, tx, evs); err != nil {
		return err
	}
	return rotateEventsTx(ctx, tx, now)
}

// sessionTarget pairs a session-owned lock's canonical path with its mode so
// the release restore guard can skip shared rows (loto-k5el.2 T4), with the
// intent the row recorded AT DELETE TIME so the caller can report what it
// actually swept rather than what it saw beforehand (loto-lzap), and with the
// worktree it was acquired from so a sweep can tell a row minted by a
// DIFFERENT worktree apart from its own, even when owner and session ids
// collapse onto one value (loto-19bz).
type sessionTarget struct {
	Canonical string
	Mode      string
	Intent    string
	Worktree  string
	ExpiresNs int64 // expires_at, so a TTL-lapsed foreign row reads dead, not ambiguous
}

// loadSessionTargetsTx returns canonical paths + modes for all locks owned by
// agent (and optionally scoped to session). Returns them in deterministic order.
// onlyIntent, when non-empty, narrows the set to rows recording exactly that
// intent. The filter belongs in this query — inside the release transaction —
// rather than in a caller that lists first and deletes second: N lanes of one
// Claude Code session share an owner id and can share a session id, so a
// same-owner peer re-acquiring a target between a caller's list and its delete
// upserts its own intent onto that row (insertOrRefreshLock's ON CONFLICT), and
// a delete keyed on target and owner alone then takes the peer's lock — the
// exact loss --only-intent exists to prevent (loto-lzap).
func loadSessionTargetsTx(ctx context.Context, tx *sql.Tx, byAgent, sessionUUID, onlyIntent string) ([]sessionTarget, error) {
	q := `SELECT target_canonical, mode, intent, worktree, expires_at FROM locks WHERE owner_uuid = ?`
	args := []any{byAgent}
	if sessionUUID != "" {
		q += andSessionClause
		args = append(args, sessionUUID)
	}
	if onlyIntent != "" {
		q += ` AND intent = ?`
		args = append(args, onlyIntent)
	}
	q += ` ORDER BY target_canonical`
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionTarget
	for rows.Next() {
		var c sessionTarget
		if err := rows.Scan(&c.Canonical, &c.Mode, &c.Intent, &c.Worktree, &c.ExpiresNs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AmbiguousHold is one lock or claim ReleaseBySession refused to sweep
// because the row was stamped from a worktree (checkout toplevel) other than
// the caller's own. The owner (and, per loto-81n, sometimes the session) id
// matched — that part of identity collapsed onto one value across sibling
// subagents that inherit one Claude Code session's env — but the worktree a
// row was taken from is read from the caller's cwd, independent of that
// shared env: the one signal available, without a harness change, that the
// row belongs to a sibling in another worktree (loto-19bz). Two siblings in
// ONE worktree still collapse; that residual needs a per-subagent identity.
type AmbiguousHold struct {
	Kind      string // "lock" or "claim"
	Canonical string // lock target or claim prefix
	Worktree  string
}

// sessionAmbiguityTx collects every lock in canonicals and — on an unfiltered
// sweep, the only one that deletes claims — every matching claim that was
// stamped from a foreign worktree. Unknown on either side is never evidence
// of a mismatch (domain.SameWorktree): a legacy row, or a caller whose git
// lookup failed, must not turn every --all into a refusal.
//
// A foreign row whose TTL lapsed is dead, not ambiguous (Codex #371 P1):
// stale rows are reclaimed lazily, and counting one would wedge every later
// SessionEnd sweep of this worktree. Such a lock is dropped from the returned
// canonicals — left for lazy reclaim, since releasing it from here would
// restore the write bit on THIS worktree's copy of the path — while such a
// claim (no filesystem effect) is left for deleteClaimsBySessionTx to sweep.
func sessionAmbiguityTx(ctx context.Context, tx *sql.Tx, canonicals []sessionTarget, byAgent, session, onlyIntent, callerWorktree string, now time.Time) ([]sessionTarget, []AmbiguousHold, error) {
	if callerWorktree == "" {
		return canonicals, nil, nil
	}
	var out []AmbiguousHold
	kept := canonicals[:0]
	for _, c := range canonicals {
		switch {
		case domain.SameWorktree(callerWorktree, c.Worktree):
			kept = append(kept, c)
		case now.UnixNano() < c.ExpiresNs:
			out = append(out, AmbiguousHold{Kind: "lock", Canonical: c.Canonical, Worktree: c.Worktree})
		}
	}
	if onlyIntent != "" {
		return kept, out, nil
	}
	claims, err := foreignClaimsTx(ctx, tx, byAgent, session, callerWorktree, now)
	if err != nil {
		return nil, nil, err
	}
	return kept, append(out, claims...), nil
}

// andSessionClause narrows an owner-scoped query to one pinned session.
const andSessionClause = ` AND session_uuid = ?`

// foreignClaimsTx returns the claims deleteClaimsBySessionTx would delete for
// (byAgent, session) whose worktree is known and differs from callerWorktree
// and whose lease has not lapsed (domain.ClaimRecord.Expired's boundary).
func foreignClaimsTx(ctx context.Context, tx *sql.Tx, byAgent, session, callerWorktree string, now time.Time) ([]AmbiguousHold, error) {
	q := `SELECT path_prefix, worktree FROM claims WHERE owner_uuid = ? AND worktree != '' AND worktree != ? AND expires_at > ?`
	args := []any{byAgent, callerWorktree, now.UnixNano()}
	if session != "" {
		q += andSessionClause
		args = append(args, session)
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY path_prefix`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AmbiguousHold
	for rows.Next() {
		h := AmbiguousHold{Kind: "claim"}
		if err := rows.Scan(&h.Canonical, &h.Worktree); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
