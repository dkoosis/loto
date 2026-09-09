package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

// A fresh store reports every counter at zero and no top-locked-bytes rows —
// the empty-store baseline `loto stats` and doctor's first line both render
// from.
func TestReadEnforcementStats_EmptyStore(t *testing.T) {
	s := mustOpen(t)
	st, err := s.ReadEnforcementStats(context.Background(), 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.RefRefused != 0 || st.RefRefusedOverridden != 0 || st.TreeChangeReported != 0 ||
		st.TreeChangeActed != 0 || st.HookCalls != 0 || st.PostMissing != 0 || st.PostMissingResolved != 0 {
		t.Errorf("nonzero counter on a fresh store: %+v", st)
	}
	if len(st.TopLockedBytes) != 0 {
		t.Errorf("top locked bytes on a fresh store: %+v", st.TopLockedBytes)
	}
}

// Seeded rows over a 30-day window, hand-counted, exercise every one of the
// four §10b counters — the AC's "seeded events over 30 days" case.
func TestReadEnforcementStats_SeededWindow(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()
	day := func(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }

	seed := func(kind, reason, detail string, at time.Time) {
		if _, err := s.AppendEvent(ctx, mustEvent(kind, reason, detail, at)); err != nil {
			t.Fatalf("seed %s: %v", kind, err)
		}
	}

	// Row 1: 4 refusals, 1 override, spread over the window.
	for i, d := range []int{1, 5, 10, 20} {
		seed(EventRefRefused, "stash", "", day(d))
		_ = i
	}
	seed(EventRefRefusedOverridden, "", "", day(5))

	// Row 2: 10 reported, 3 acted.
	for i := range 10 {
		seed(EventTreeChangeReported, "rule", "", day(i%25+1))
	}
	for i := range 3 {
		seed(EventTreeChangeActed, "rule", "", day(i+1))
	}

	// Row 3: two calls, each with a pre and a post row.
	seed(EventHookTiming, "pre", `{"call_id":"c1","pre_ms":10,"locked_bytes":100}`, day(2))
	seed(EventHookTiming, "post", `{"call_id":"c1","post_ms":20,"locked_bytes":100}`, day(2))
	seed(EventHookTiming, "pre", `{"call_id":"c2","pre_ms":30,"locked_bytes":500}`, day(3))
	seed(EventHookTiming, "post", `{"call_id":"c2","post_ms":40,"locked_bytes":500}`, day(3))

	// Row 4: 2 post_missing, 1 resolved (age 5000ms).
	seed(EventPostMissing, "", `{"call_id":"m1","age_ms":1000}`, day(1))
	seed(EventPostMissing, "", `{"call_id":"m2","age_ms":2000}`, day(2))
	seed(EventPostMissingResolved, "posted", `{"call_id":"m1","age_ms":5000}`, day(1))

	// One row outside the 30-day window, excluded from every count below.
	seed(EventRefRefused, "stash", "", day(31))

	st, err := s.ReadEnforcementStats(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if st.RefRefused != 4 {
		t.Errorf("ref_refused = %d, want 4 (the day-31 row is outside the window)", st.RefRefused)
	}
	if st.RefRefusedOverridden != 1 {
		t.Errorf("ref_overridden = %d, want 1", st.RefRefusedOverridden)
	}
	if st.TreeChangeReported != 10 {
		t.Errorf("tree_reported = %d, want 10", st.TreeChangeReported)
	}
	if st.TreeChangeActed != 3 {
		t.Errorf("tree_acted = %d, want 3", st.TreeChangeActed)
	}
	if st.HookCalls != 2 {
		t.Errorf("hook_calls = %d, want 2", st.HookCalls)
	}
	// c1 totals 30ms, c2 totals 70ms; p50 (nearest-rank over [30,70]) is 30,
	// p99 is 70.
	if st.HookCallP50 != 30*time.Millisecond {
		t.Errorf("p50 = %s, want 30ms", st.HookCallP50)
	}
	if st.HookCallP99 != 70*time.Millisecond {
		t.Errorf("p99 = %s, want 70ms", st.HookCallP99)
	}
	if len(st.TopLockedBytes) != 2 || st.TopLockedBytes[0].CallID != "c2" || st.TopLockedBytes[0].LockedBytes != 500 {
		t.Errorf("top locked bytes = %+v, want c2:500 first", st.TopLockedBytes)
	}
	if st.PostMissing != 2 {
		t.Errorf("post_missing = %d, want 2", st.PostMissing)
	}
	if st.PostMissingResolved != 1 {
		t.Errorf("post_missing_resolved = %d, want 1", st.PostMissingResolved)
	}
	if st.ResolvedMeanAge != 5000*time.Millisecond {
		t.Errorf("resolved mean age = %s, want 5000ms", st.ResolvedMeanAge)
	}
}

// --since excludes rows older than the window, same rule ReadGateStats
// already honors.
func TestReadEnforcementStats_HonorsTheWindow(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if _, err := s.AppendEvent(ctx, mustEvent(EventRefRefused, "stash", "", time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE events SET created_at = ? WHERE event_kind = ?`,
		time.Now().Add(-8*24*time.Hour).UnixNano(), EventRefRefused); err != nil {
		t.Fatal(err)
	}

	st, err := s.ReadEnforcementStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.RefRefused != 0 {
		t.Errorf("ref_refused = %d, want 0 — the row is outside --since 7d", st.RefRefused)
	}
}

func mustEvent(kind, reason, detail string, at time.Time) domain.Event {
	return domain.Event{Kind: kind, ActorUUID: tcAlice, Reason: reason, Detail: detail, CreatedAt: at}
}
