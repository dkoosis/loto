package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/gate"
)

const tcCandA = "c-aaaa1111"

// Created-path attribution fixtures. The blobs are shape-correct (40 hex) but
// never resolved — this package records SHAs, it hashes nothing.
const (
	tcPathA = "pkg/a.go"
	tcPathB = "pkg/b.go"
	tcBlobA = "1111111111111111111111111111111111111111"
	tcBlobB = "2222222222222222222222222222222222222222"
)

func TestRecordAdmissionVerdict_CountsPerClass(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range []gate.RejectionReason{
		gate.ReasonViolationIntersect, gate.ReasonViolationIntersect, gate.ReasonStalePreimage,
	} {
		if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, r, nil); err != nil {
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

// The created write-set survives the round trip through events.detail: each
// path keyed to the candidate that created it AND the blob it wrote there —
// the attribution `loto sync` deletes residue by (loto-ovno.13).
func TestRecordAdmissionVerdict_CreatedPathsRoundTrip(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, gate.ReasonStalePreimage,
		[]gate.CreatedPath{{Path: tcPathB, Blob: tcBlobB}, {Path: tcPathA, Blob: tcBlobA}}); err != nil {
		t.Fatal(err)
	}

	got, err := s.RejectedCandidateCreations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for p, wantBlob := range map[string]string{tcPathA: tcBlobA, tcPathB: tcBlobB} {
		if got[p].CandidateID != tcCandA {
			t.Errorf("created path %s attributed to %q, want %q", p, got[p].CandidateID, tcCandA)
		}
		if got[p].Blob != wantBlob {
			t.Errorf("created path %s blob = %q, want %q", p, got[p].Blob, wantBlob)
		}
	}
	if len(got) != 2 {
		t.Errorf("attribution map has %d entries, want 2: %v", len(got), got)
	}
}

// An ACCEPTED candidate's created paths are never recorded, whatever the
// caller passes. Those files are live work headed for promotion; an audit row
// that made them attributable to a deleter would be a loaded gun.
func TestRecordAdmissionVerdict_AcceptedCandidateRecordsNoCreatedPaths(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, "",
		[]gate.CreatedPath{{Path: "pkg/live.go", Blob: tcBlobA}}); err != nil {
		t.Fatal(err)
	}

	got, err := s.RejectedCandidateCreations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("an accepted candidate's paths became deletable: %v", got)
	}
}

// A created path with no blob is dropped at write time: it could never be
// content-checked before deletion, so recording it would only hand sync an
// attribution it must refuse.
func TestRecordAdmissionVerdict_DropsCreatedPathWithoutBlob(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, gate.ReasonStalePreimage,
		[]gate.CreatedPath{{Path: tcPathA}, {Path: tcPathB, Blob: tcBlobB}}); err != nil {
		t.Fatal(err)
	}

	got, err := s.RejectedCandidateCreations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[tcPathA]; ok {
		t.Errorf("a blobless created path became attributable: %v", got)
	}
	if got[tcPathB].Blob != tcBlobB {
		t.Errorf("the well-formed entry was lost with it: %v", got)
	}
}

// A row written in the pre-blob payload shape — bare path strings — is
// unreadable now, and unreadable means unattributed. Deliberately fail-closed
// by construction: no version field to check, no legacy branch to get wrong.
func TestRejectedCandidateCreations_LegacyPayloadShapeIsUnattributed(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, gate.ReasonStalePreimage,
		[]gate.CreatedPath{{Path: tcPathA, Blob: tcBlobA}}); err != nil {
		t.Fatal(err)
	}
	// Rewrite the row's payload as the old shape this bead first shipped.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE events SET detail = ? WHERE event_kind = ?`,
		`{"created":["pkg/a.go"]}`, EventCandidateRejected); err != nil {
		t.Fatal(err)
	}

	got, err := s.RejectedCandidateCreations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a pre-blob payload produced attribution: %v", got)
	}
}

// A rejection with nothing to declare — a capture failure, where no envelope
// was ever built — records no payload, and its residue stays unattributed.
func TestRejectedCandidateCreations_NoPayloadIsNoAttribution(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, gate.ReasonStaleAncestry, nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.RejectedCandidateCreations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a payload-free rejection produced attribution: %v", got)
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
	if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, gate.ReasonStaleAncestry, nil); err != nil {
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
	if err := s.RecordAdmissionVerdict(ctx, tcAlice, tcCandA, gate.RejectionReason("from-the-future"), nil); err != nil {
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
