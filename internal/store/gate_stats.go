package store

import (
	"context"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
)

// Admission verdict event kinds (loto-ovno.9). Every candidate that reaches a
// verdict leaves exactly one of these, so the rejection taxonomy stops being
// a thing the CLI prints once and becomes a thing the repo can count.
//
// ‡ The rejection CLASS rides in Event.Reason, not in the kind: a kind per
// class would need a CHECK-constraint migration every time the taxonomy
// grows, and would make "how many candidates were rejected at all" a query
// over a list someone has to remember to extend.
const (
	EventCandidateAccepted = "candidate_accepted"
	EventCandidateRejected = "candidate_rejected"
)

// RecordAdmissionVerdict appends the one audit row a judged candidate owes.
// Target is the candidate id rather than a path: a verdict is a fact about a
// candidate, and its write-set may be many paths or (on a malformed
// candidate) none the store can trust.
//
// Best-effort, like every other audit append: a lost breadcrumb must not undo
// a verdict that has already been acted on. The caller surfaces the error;
// it does not retry.
func (s *Store) RecordAdmissionVerdict(ctx context.Context, actorUUID, candidateID string, reason gate.RejectionReason) error {
	kind := EventCandidateAccepted
	if reason != "" {
		kind = EventCandidateRejected
	}
	return s.appendAuditDetached([]domain.Event{{
		Kind:      kind,
		Target:    domain.Target{Canonical: candidateID},
		ActorUUID: actorUUID,
		Reason:    string(reason),
		CreatedAt: time.Now(),
	}})
}

// GateStats is what `loto gate stats` reports over one window.
type GateStats struct {
	Since    time.Duration
	Accepted int
	Rejected int
	Bypassed int
	// ByClass counts every taxonomy class, including the ones at zero.
	// Keyed by class so the reporter can iterate gate.RejectionReasons and
	// print a row per class in taxonomy order rather than in map order.
	ByClass map[gate.RejectionReason]int
}

// Total is every judged candidate in the window — accepted plus rejected.
// Bypasses are NOT added in: a bypassed candidate was never judged, which is
// the entire point of counting them separately.
func (g GateStats) Total() int { return g.Accepted + g.Rejected }

// ReadGateStats aggregates admission verdicts over the last `since`.
//
// ‡ The window is bounded by events retention (1000 rows / 7 days,
// events.go), which is a real ceiling and not a bug to route around here: a
// `--since 30d` on a busy repo reports what the audit trail still holds, and
// the reporter says which window it read. Widening the horizon is a retention
// decision, not a query one.
func (s *Store) ReadGateStats(ctx context.Context, since time.Duration) (GateStats, error) {
	out := GateStats{Since: since, ByClass: map[gate.RejectionReason]int{}}
	for _, r := range gate.RejectionReasons {
		out.ByClass[r] = 0
	}

	cutoff := time.Now().Add(-since).UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_kind, reason, COUNT(*) FROM events
		  WHERE created_at >= ? AND event_kind IN (?, ?, ?)
		  GROUP BY event_kind, reason`,
		cutoff, EventCandidateAccepted, EventCandidateRejected, EventGateBypass)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	for rows.Next() {
		var kind, reason string
		var n int
		if err := rows.Scan(&kind, &reason, &n); err != nil {
			return out, err
		}
		switch kind {
		case EventCandidateAccepted:
			out.Accepted += n
		case EventCandidateRejected:
			out.Rejected += n
			// An unrecognized class still counts toward Rejected — the total
			// must never disagree with the sum of what happened just because
			// a newer loto wrote a class this binary does not know.
			out.ByClass[gate.RejectionReason(reason)] += n
		case EventGateBypass:
			out.Bypassed += n
			out.ByClass[gate.ReasonGateBypass] += n
		}
	}
	return out, rows.Err()
}
