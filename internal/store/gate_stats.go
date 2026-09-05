package store

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
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

// verdictDetail is the JSON payload a candidate_rejected event carries in
// events.detail. Created is the subset of the candidate's declared write-set
// whose paths did not exist in integration — the ONLY paths whose untracked
// worktree residue can be attributed to this rejection, and therefore the only
// ones `loto sync` may delete (loto-ovno.13). A modified path's residue is a
// fast-forward, never a deletion; a created path's is a file integration has
// no entry for at all.
type verdictDetail struct {
	Created []string `json:"created"`
}

// RecordAdmissionVerdict appends the one audit row a judged candidate owes.
// Target is the candidate id rather than a path: a verdict is a fact about a
// candidate, and its write-set may be many paths or (on a malformed
// candidate) none the store can trust.
//
// createdPaths is the rejected candidate's created write-set (gate.Envelope's
// CreatedPaths). It is recorded ONLY on a rejection: an accepted candidate's
// created files are live work, and a record that made them attributable to a
// deleter would turn this audit row into a loaded gun. The kind switch below
// is the enforcement — a caller passing paths with an empty reason gets an
// accepted event with no payload, not an accepted event sync can act on.
//
// Best-effort, like every other audit append: a lost breadcrumb must not undo
// a verdict that has already been acted on. The caller surfaces the error;
// it does not retry. A lost breadcrumb costs attribution, which fails closed:
// the residue is reported unattributed and left on disk.
func (s *Store) RecordAdmissionVerdict(ctx context.Context, actorUUID, candidateID string, reason gate.RejectionReason, createdPaths []string) error {
	kind := EventCandidateAccepted
	detail := ""
	if reason != "" {
		kind = EventCandidateRejected
		var err error
		if detail, err = encodeVerdictDetail(createdPaths); err != nil {
			return err
		}
	}
	return s.appendAuditDetached([]domain.Event{{
		Kind:      kind,
		Target:    domain.Target{Canonical: candidateID},
		ActorUUID: actorUUID,
		Reason:    string(reason),
		Detail:    detail,
		CreatedAt: time.Now(),
	}})
}

// encodeVerdictDetail renders the created-path payload, sorted and
// deduplicated so the same rejection always serializes to the same bytes. No
// paths means an EMPTY payload, not `{"created":null}` — the
// unattributed case must be indistinguishable from a pre-column row, since
// both mean the same thing to sync: delete nothing.
func encodeVerdictDetail(createdPaths []string) (string, error) {
	if len(createdPaths) == 0 {
		return "", nil
	}
	uniq := append([]string(nil), createdPaths...)
	sort.Strings(uniq)
	uniq = slices.Compact(uniq)
	b, err := json.Marshal(verdictDetail{Created: uniq})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// RejectedCandidateCreations maps each repo-relative path a REJECTED candidate
// recorded as created to that candidate's id, over the rejections the events
// table still holds (EventsRetentionAge / EventsRetentionMaxRows — that bound
// IS the attribution horizon, and the caller prints it).
//
// ‡ Only candidate_rejected rows are read. An accepted candidate never carries
// a payload (RecordAdmissionVerdict above), so no query filter alone is load-
// bearing here — but the filter is written anyway, because a reader that
// deletes files must not depend on a writer's discipline it cannot see.
//
// A path claimed by more than one rejection resolves to the MOST RECENT one:
// a resubmit that was rejected again is the honest attribution, and ordering
// the scan by (created_at, id) makes the winner deterministic. A row whose
// detail does not parse is skipped, not guessed at — an unreadable payload
// leaves its paths unattributed, which leaves them on disk.
func (s *Store) RejectedCandidateCreations(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT target_canonical, detail FROM events
		  WHERE event_kind = ? AND detail <> ''
		  ORDER BY created_at, id`, EventCandidateRejected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var candidateID, detail string
		if err := rows.Scan(&candidateID, &detail); err != nil {
			return nil, err
		}
		var d verdictDetail
		if err := json.Unmarshal([]byte(detail), &d); err != nil {
			continue
		}
		for _, p := range d.Created {
			if p == "" {
				continue
			}
			out[p] = candidateID
		}
	}
	return out, rows.Err()
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
