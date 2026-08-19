package store

import (
	"context"
	"errors"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
)

// AcceptCandidate runs the "on accept" actions git-gate.md names: convert
// env's write-set leases into durable candidate claims, then write the
// envelope + proposal commit under refs/loto/* in one atomic transaction.
//
// The claims go first and carry a guard: admission judged on an epoch map read
// before it ran, so the leases are re-checked inside the same op-flock'd tx
// that inserts the claims (loto-ovno.10). A submit that lost its lease in that
// window fails here with a *LeaseRevalidationError and leaves no refs behind.
//
// ‡ Caller's job, not this function's: calling gate.Admit and confirming the
// decision is Accepted, and supplying the liveness probe the claim guard
// judges leases with. AcceptCandidate trusts env is already admitted —
// mirroring InsertCandidateClaims' own "the store does not decide, it
// persists what the caller decided" posture (candidate_claims.go).
//
// ‡ The lock/lease this candidate was captured under is deliberately left
// alone. The durable claim is an ADDITIONAL, longer-lived protection — not a
// replacement — so a rejected candidate leaves the original lock exactly as
// it was, and the proposer's own lock lifecycle (refresh/release/expire)
// keeps working independently of admission. Nothing in git-gate.md's
// "on accept" language says to release the lease; "lease → durable claim"
// reads as "the lease's authorization now ALSO backs a claim," not "the
// lease is spent."
func (s *Store) AcceptCandidate(ctx context.Context, repoTop string, env gate.Envelope, submitter domain.CandidateClaim, live domain.HolderLiveProbe) (envelopeSHA string, err error) {
	now := time.Now()
	claims := make([]domain.CandidateClaim, len(env.WriteSet))
	for i, path := range env.WriteSet {
		claims[i] = domain.CandidateClaim{
			PathCanonical: path,
			CandidateID:   env.CandidateID,
			OwnerUUID:     submitter.OwnerUUID,
			SessionUUID:   submitter.SessionUUID,
			CreatedAt:     now,
			Host:          submitter.Host,
			PID:           submitter.PID,
			ProcStart:     submitter.ProcStart,
		}
	}
	// ‡ Claims FIRST, refs second (loto-ovno.10). The claim insert is the step
	// that can still lose — it revalidates the leases under the op-flock, so a
	// lease lost in the admission window is refused HERE, not earlier. Writing
	// the candidate refs first would leave refs/loto/candidates/<id> behind on
	// every lost race; writing them after leaves nothing. The inverse leak —
	// claims held for a candidate whose refs never landed — is closed by
	// releasing them on the ref-write failure path below, and is conservative
	// in the meantime: an unresolved claim only ever BLOCKS a new lease.
	guard := ClaimGuard{Owner: submitter.OwnerUUID, Epoch: env.LeaseEpoch, Live: live}
	if err := s.InsertCandidateClaims(ctx, claims, guard); err != nil {
		return "", err
	}

	envelopeSHA, err = env.WriteBlob(ctx, repoTop)
	if err != nil {
		return "", errors.Join(err, s.releaseCandidateClaimsForPathsDetached(env.CandidateID, env.WriteSet))
	}
	if err := gate.WriteCandidateRefs(ctx, repoTop, env.CandidateID, envelopeSHA, env.ProposalSHA); err != nil {
		return "", errors.Join(err, s.releaseCandidateClaimsForPathsDetached(env.CandidateID, env.WriteSet))
	}
	return envelopeSHA, nil
}

// RecordGateBypass appends the audit event LOTO_GATE=off owes every time it
// fires (git-gate.md "The gate can never become the outage": "every bypass is
// logged as a violation-class event"). No Target — a bypass is a
// session-scoped fact, not a per-path one.
func (s *Store) RecordGateBypass(ctx context.Context, actorUUID, reason string) error {
	return s.appendAuditDetached([]domain.Event{{
		Kind:      EventGateBypass,
		ActorUUID: actorUUID,
		Reason:    reason,
		CreatedAt: time.Now(),
	}})
}
