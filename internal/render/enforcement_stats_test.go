package render

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// An empty store renders five lines, all ✓: the triage line plus one row per
// §10b question, none of them past its threshold.
func TestEmitEnforcementStats_EmptyStoreIsAllPass(t *testing.T) {
	var buf bytes.Buffer
	EmitEnforcementStats(&buf, EnforcementStats{Since: 30 * 24 * time.Hour})
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5:\n%s", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[0], "✓ enforcement-stats") {
		t.Errorf("triage line = %q, want a ✓ enforcement-stats prefix", lines[0])
	}
	for i, l := range lines[1:] {
		if !strings.HasPrefix(l, "✓ ") {
			t.Errorf("row %d = %q, want a ✓ prefix on an empty store", i+1, l)
		}
	}
}

// Row 1 flags when overrides have caught up to refusals (loto-ea8y.8's
// recorded decision: ratio ≥ 1.0).
func TestEmitEnforcementStats_Row1FlagsOverridesApproachingRefusals(t *testing.T) {
	var buf bytes.Buffer
	EmitEnforcementStats(&buf, EnforcementStats{Since: time.Hour, RefRefused: 4, RefRefusedOverridden: 4})
	lines := strings.Split(buf.String(), "\n")
	if !strings.HasPrefix(lines[1], "⚠ i1_refusals") {
		t.Errorf("row 1 = %q, want ⚠ (overrides == refusals)", lines[1])
	}
	if !strings.HasPrefix(lines[0], "⚠") {
		t.Errorf("triage glyph did not roll up row 1's warning: %q", lines[0])
	}
}

func TestEmitEnforcementStats_Row1PassesWhenOverridesAreRare(t *testing.T) {
	var buf bytes.Buffer
	EmitEnforcementStats(&buf, EnforcementStats{Since: time.Hour, RefRefused: 10, RefRefusedOverridden: 1})
	lines := strings.Split(buf.String(), "\n")
	if !strings.HasPrefix(lines[1], "✓ i1_refusals") {
		t.Errorf("row 1 = %q, want ✓", lines[1])
	}
}

// Row 2 flags outside the open band (< 1/10 or >= 1/2); inside it, ✓.
func TestEmitEnforcementStats_Row2ThresholdBands(t *testing.T) {
	cases := []struct {
		name            string
		reported, acted int
		wantWarn        bool
	}{
		{"below one-tenth", 100, 5, true},
		{"at one-half", 10, 5, true},
		{"above one-half", 10, 8, true},
		{"open band", 10, 3, false},
		{"no reports yet", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			EmitEnforcementStats(&buf, EnforcementStats{Since: time.Hour, TreeChangeReported: tc.reported, TreeChangeActed: tc.acted})
			lines := strings.Split(buf.String(), "\n")
			want := "✓ drift_acted"
			if tc.wantWarn {
				want = "⚠ drift_acted"
			}
			if !strings.HasPrefix(lines[2], want) {
				t.Errorf("row 2 = %q, want prefix %q", lines[2], want)
			}
		})
	}
}

// Row 3 flags exactly on §10b's own thresholds: p99 > 250ms or p50 > 50ms.
func TestEmitEnforcementStats_Row3HookCostThresholds(t *testing.T) {
	cases := []struct {
		name     string
		p50, p99 time.Duration
		wantWarn bool
	}{
		{"under both", 10 * time.Millisecond, 100 * time.Millisecond, false},
		{"p99 over", 10 * time.Millisecond, 251 * time.Millisecond, true},
		{"p50 over", 51 * time.Millisecond, 100 * time.Millisecond, true},
		{"at the p99 boundary", 10 * time.Millisecond, 250 * time.Millisecond, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			EmitEnforcementStats(&buf, EnforcementStats{Since: time.Hour, HookCallP50: tc.p50, HookCallP99: tc.p99})
			lines := strings.Split(buf.String(), "\n")
			want := "✓ hook_cost"
			if tc.wantWarn {
				want = "⚠ hook_cost"
			}
			if !strings.HasPrefix(lines[3], want) {
				t.Errorf("row 3 = %q, want prefix %q", lines[3], want)
			}
		})
	}
}

// Row 4 flags at more than one per week — exactly §10b's own wording.
func TestEmitEnforcementStats_Row4PerWeekThreshold(t *testing.T) {
	var buf bytes.Buffer
	EmitEnforcementStats(&buf, EnforcementStats{Since: 7 * 24 * time.Hour, PostMissing: 2})
	lines := strings.Split(buf.String(), "\n")
	if !strings.HasPrefix(lines[4], "⚠ post_missing") {
		t.Errorf("row 4 = %q, want ⚠ (2/week > 1)", lines[4])
	}

	buf.Reset()
	EmitEnforcementStats(&buf, EnforcementStats{Since: 7 * 24 * time.Hour, PostMissing: 1})
	lines = strings.Split(buf.String(), "\n")
	if !strings.HasPrefix(lines[4], "✓ post_missing") {
		t.Errorf("row 4 = %q, want ✓ (1/week is not > 1)", lines[4])
	}
}

// Deterministic: identical input renders byte-identical output.
func TestEmitEnforcementStats_Deterministic(t *testing.T) {
	in := EnforcementStats{
		Since: 24 * time.Hour, RefRefused: 3, RefRefusedOverridden: 1,
		TreeChangeReported: 5, TreeChangeActed: 1,
		HookCallP50: 10 * time.Millisecond, HookCallP99: 40 * time.Millisecond, HookCalls: 2,
		TopLockedBytes: []HookTimingTop{{CallID: "c2", LockedBytes: 500}, {CallID: "c1", LockedBytes: 100}},
		PostMissing:    1, PostMissingResolved: 1, ResolvedMeanAge: 5 * time.Second,
	}
	var a, b bytes.Buffer
	EmitEnforcementStats(&a, in)
	EmitEnforcementStats(&b, in)
	if a.String() != b.String() {
		t.Errorf("non-deterministic output:\n%s\n---\n%s", a.String(), b.String())
	}
}
