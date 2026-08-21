package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/gate"
)

const tcCandA = "c-aaaa1111"

func TestRecordAdmissionVerdict_CountsPerClass(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, ""); err != nil {
		t.Fatal(err)
	}
	for _, r := range []gate.RejectionReason{
		gate.ReasonViolationIntersect, gate.ReasonViolationIntersect, gate.ReasonStalePreimage,
	} {
		if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, r); err != nil {
			t.Fatal(err)
		}
	}

	st, err := s.ReadGateStats(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.Accepted != 1 || st.Rejected != 3 || st.Total() != 4 {
		t.Errorf("accepted=%d rejected=%d total=%d, want 1/3/4", st.Accepted, st.Rejected, st.Total())
	}
	if got := st.ByClass[gate.ReasonViolationIntersect]; got != 2 {
		t.Errorf("violation-intersect = %d, want 2", got)
	}
	if got := st.ByClass[gate.ReasonStalePreimage]; got != 1 {
		t.Errorf("stale-preimage = %d, want 1", got)
	}
}

// Every taxonomy class is present in the map even at zero — the reporter
// prints them, and "this class has never fired here" is the reading that
// decides whether the contamination story needs a stronger mechanism.
func TestReadGateStats_EnumeratesEveryClassIncludingZero(t *testing.T) {
	s := mustOpen(t)

	st, err := s.ReadGateStats(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range gate.RejectionReasons {
		if _, ok := st.ByClass[r]; !ok {
			t.Errorf("class %s missing from a fresh report", r)
		}
	}
	if st.Total() != 0 {
		t.Errorf("total = %d on a fresh store, want 0", st.Total())
	}
}

// A bypass is counted apart from a verdict: LOTO_GATE=off admitted a
// candidate the gate never judged, and folding it into "accepted" would make
// the escape hatch invisible in exactly the report meant to expose it.
func TestReadGateStats_BypassIsItsOwnClassNotAnAcceptance(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RecordGateBypass(ctx, tcAlice, "test"); err != nil {
		t.Fatal(err)
	}

	st, err := s.ReadGateStats(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.Bypassed != 1 {
		t.Errorf("bypassed = %d, want 1", st.Bypassed)
	}
	if st.Accepted != 0 || st.Total() != 0 {
		t.Errorf("a bypass was counted as a judged candidate: accepted=%d total=%d", st.Accepted, st.Total())
	}
	if got := st.ByClass[gate.ReasonGateBypass]; got != 1 {
		t.Errorf("gate-bypass class = %d, want 1", got)
	}
}

// The window is a window: an event older than --since is out of the report.
func TestReadGateStats_HonorsTheWindow(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, gate.ReasonStaleAncestry); err != nil {
		t.Fatal(err)
	}
	// Age the row past the window rather than sleeping through it.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE events SET created_at = ? WHERE event_kind = ?`,
		time.Now().Add(-48*time.Hour).UnixNano(), EventCandidateRejected); err != nil {
		t.Fatal(err)
	}

	st, err := s.ReadGateStats(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rejected != 0 {
		t.Errorf("rejected = %d, want 0 — the row is outside the window", st.Rejected)
	}
}

// A class this binary does not know still counts toward the total: the
// headline must never disagree with what actually happened just because a
// newer loto wrote a class name this one has no constant for.
func TestReadGateStats_UnknownClassStillCountsAsRejected(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, gate.RejectionReason("from-the-future")); err != nil {
		t.Fatal(err)
	}

	st, err := s.ReadGateStats(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rejected != 1 || st.Total() != 1 {
		t.Errorf("rejected=%d total=%d, want 1/1", st.Rejected, st.Total())
	}
}
