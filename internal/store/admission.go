package store

import (
	"context"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
)

// AcceptCandidate runs the "on accept" actions git-gate.md names: write the
// envelope + proposal commit under refs/loto/* in one atomic transaction, then
// convert env's write-set leases into durable candidate claims.
//
// ‡ Caller's job, not this function's: calling gate.Admit and confirming the
// decision is Accepted. AcceptCandidate trusts env is already admitted —
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
func (s *Store) AcceptCandidate(ctx context.Context, repoTop string, env gate.Envelope, submitter domain.CandidateClaim) (envelopeSHA string, err error) {
	envelopeSHA, err = env.WriteBlob(ctx, repoTop)
	if err != nil {
		return "", err
	}
	if err := gate.WriteCandidateRefs(ctx, repoTop, env.CandidateID, envelopeSHA, env.ProposalSHA); err != nil {
		return "", err
	}

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
	if err := s.InsertCandidateClaims(ctx, claims); err != nil {
		return "", err
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
