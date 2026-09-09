package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"loto/internal/domain"
)

// MarkPostMissing writes one post_missing event per call it flags, in the
// same tx as the flag itself — §10b row 4's numerator, loto-ea8y.8.
func TestMarkPostMissing_WritesOneEventPerFlaggedCall(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	const tReport = 10 * time.Minute
	t0 := time.Now()

	mustPre(t, s, "call-a", t0, hookObs(tcHookPath, tcSHA1))
	mustPre(t, s, "call-b", t0.Add(time.Second), hookObs(tcHookPath, tcSHA1))

	n, err := s.MarkPostMissing(ctx, t0.Add(tReport+time.Minute), tReport)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("flagged %d calls, want 2", n)
	}

	evs, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var missing []domain.Event
	for _, e := range evs {
		if e.Kind == EventPostMissing {
			missing = append(missing, e)
		}
	}
	if len(missing) != 2 {
		t.Fatalf("post_missing events = %d, want 2: %+v", len(missing), missing)
	}
	for _, e := range missing {
		if e.Target.Canonical != "call-a" && e.Target.Canonical != "call-b" {
			t.Errorf("unexpected target %q", e.Target.Canonical)
		}
		var d postMissingDetail
		if err := json.Unmarshal([]byte(e.Detail), &d); err != nil {
			t.Fatalf("detail %q: %v", e.Detail, err)
		}
		if d.CallID != e.Target.Canonical {
			t.Errorf("detail call_id %q != target %q", d.CallID, e.Target.Canonical)
		}
		if d.AgeMS <= 0 {
			t.Errorf("age_ms = %d, want > 0", d.AgeMS)
		}
	}

	// A second sweep must not re-flag or re-write: post_missing = 0 is the
	// WHERE clause's guard, and RowsAffected/len(toFlag) both key off it.
	n2, err := s.MarkPostMissing(ctx, t0.Add(2*tReport), tReport)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second sweep flagged %d, want 0 (already flagged)", n2)
	}
}

// A post that lands late for a post_missing call writes post_missing_resolved
// with reason "posted" — §10b row 4's other half.
func TestRecordCallPost_ResolvesPostMissingWhenPostLandsLate(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	const tReport = 10 * time.Minute
	t0 := time.Now()

	mustPre(t, s, "call-late", t0, hookObs(tcHookPath, tcSHA1))
	if n, err := s.MarkPostMissing(ctx, t0.Add(tReport+time.Minute), tReport); err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}

	tPost := t0.Add(tReport + 2*time.Minute)
	if _, err := s.RecordCallPost(ctx, "call-late", tPost, []HookPathState{hookObs(tcHookPath, tcSHA1)}); err != nil {
		t.Fatalf("post: %v", err)
	}

	evs, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var resolved *domain.Event
	for i := range evs {
		if evs[i].Kind == EventPostMissingResolved {
			resolved = &evs[i]
		}
	}
	if resolved == nil {
		t.Fatalf("no post_missing_resolved event; events: %+v", evs)
	}
	if resolved.Reason != "posted" {
		t.Errorf("reason = %q, want posted", resolved.Reason)
	}
	if resolved.Target.Canonical != "call-late" {
		t.Errorf("target = %q, want call-late", resolved.Target.Canonical)
	}
	var d postMissingDetail
	if err := json.Unmarshal([]byte(resolved.Detail), &d); err != nil {
		t.Fatalf("detail: %v", err)
	}
	wantAge := tPost.Sub(t0).Milliseconds()
	if d.AgeMS != wantAge {
		t.Errorf("age_ms = %d, want %d", d.AgeMS, wantAge)
	}
}

// A call that posts BEFORE ever crossing T_report writes no post_missing
// pair at all — resolving something that was never flagged would invent a
// numerator §10b's ratio never had a denominator for.
func TestRecordCallPost_NoResolutionWhenNeverFlagged(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now()
	mustPre(t, s, "call-fast", t0, hookObs(tcHookPath, tcSHA1))
	if _, err := s.RecordCallPost(ctx, "call-fast", t0.Add(time.Second), []HookPathState{hookObs(tcHookPath, tcSHA1)}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == EventPostMissing || e.Kind == EventPostMissingResolved {
			t.Errorf("unexpected %s event on a call that never crossed T_report: %+v", e.Kind, e)
		}
	}
}

// A post_missing call whose owner is found dead resolves with reason
// "session_died", not "posted" — the other of §10b row 4's two resolutions.
func TestMarkDeadOwnerCalls_ResolvesPostMissingWhenOwnerDies(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	const tReport = 10 * time.Minute
	t0 := time.Now()

	if _, err := s.RecordCallPre(ctx, HookCall{
		CallID: "call-died", OwnerUUID: tcOwnerA, SessionUUID: "sess-died", TPre: t0,
	}, []HookPathState{hookObs(tcHookPath, tcSHA1)}); err != nil {
		t.Fatalf("pre: %v", err)
	}
	if n, err := s.MarkPostMissing(ctx, t0.Add(tReport+time.Minute), tReport); err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}

	deathAt := t0.Add(tReport + 5*time.Minute)
	if n, err := s.MarkDeadOwnerCalls(ctx, deathAt, func(domain.SessionUUID) bool { return true }); err != nil || n != 1 {
		t.Fatalf("mark dead: n=%d err=%v", n, err)
	}

	evs, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var resolved *domain.Event
	for i := range evs {
		if evs[i].Kind == EventPostMissingResolved {
			resolved = &evs[i]
		}
	}
	if resolved == nil {
		t.Fatalf("no post_missing_resolved event; events: %+v", evs)
	}
	if resolved.Reason != "session_died" {
		t.Errorf("reason = %q, want session_died", resolved.Reason)
	}
	if resolved.ActorUUID != string(tcOwnerA) {
		t.Errorf("actor = %q, want %q", resolved.ActorUUID, tcOwnerA)
	}
}

// A dead-owner call that was NEVER flagged post_missing resolves nothing:
// MarkDeadOwnerCalls's own ending (dead_at) is unaffected either way, but the
// §10b counter must not gain a resolution with no matching flag.
func TestMarkDeadOwnerCalls_NoResolutionWhenNeverFlagged(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now()
	if _, err := s.RecordCallPre(ctx, HookCall{
		CallID: "call-quick-death", OwnerUUID: tcOwnerA, SessionUUID: "sess-qd", TPre: t0,
	}, []HookPathState{hookObs(tcHookPath, tcSHA1)}); err != nil {
		t.Fatalf("pre: %v", err)
	}
	if n, err := s.MarkDeadOwnerCalls(ctx, t0.Add(time.Second), func(domain.SessionUUID) bool { return true }); err != nil || n != 1 {
		t.Fatalf("mark dead: n=%d err=%v", n, err)
	}
	evs, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == EventPostMissing || e.Kind == EventPostMissingResolved {
			t.Errorf("unexpected %s event: %+v", e.Kind, e)
		}
	}
}
