package render

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// EnforcementStats is the render package's own input shape for
// `loto stats` / doctor's first line — decoupled from internal/store's
// EnforcementStats the way GateStatsClass is decoupled from store.GateStats,
// so this package stays a pure formatter over primitives the caller already
// computed.
type EnforcementStats struct {
	Since time.Duration

	RefRefused           int
	RefRefusedOverridden int

	TreeChangeReported int
	TreeChangeActed    int

	HookCallP50    time.Duration
	HookCallP99    time.Duration
	HookCalls      int
	TopLockedBytes []HookTimingTop

	PostMissing         int
	PostMissingResolved int
	ResolvedMeanAge     time.Duration
}

// HookTimingTop is one call's slot in the top-3-by-locked_bytes list, caller
// order preserved (store.ReadEnforcementStats already sorts it).
type HookTimingTop struct {
	CallID      string
	LockedBytes int64
}

// enforcementRowVerdict is one §10b row's glyph and threshold text, decided by
// EmitEnforcementStats below rather than the caller, so the four rows read
// consistently regardless of who calls in (`loto stats`, doctor's first
// line).
type enforcementRowVerdict struct {
	warn      bool
	threshold string
}

// EmitEnforcementStats renders `loto stats`: triage counts on the first body
// line, then one row per enforcement-design.md §10b question, each carrying
// its number and the threshold it is judged against (this bead's Rules).
// doctor calls this with the same *EnforcementStats to print an identical
// first line — the one line the two commands are required to agree on.
//
// Each row prints ✓ or ⚠ only (design.md's ✗ is reserved for a hard failure;
// these are advisory decision points, not broken state) — the mapping this
// bead decided and recorded on loto-ea8y.8:
//
//   - row 1 (I1): ⚠ when overrides have caught up to refusals (ratio ≥ 1.0)
//     — "override≈refusal" read as equality, since §10b gives no looser band
//     and a looser one would be an invented constant with no evidence behind
//     it.
//   - row 2 (drift): ⚠ when the acted/reported ratio has left the
//     "still gathering data" band — below 1/10 (don't build) or at/above 1/2
//     (build) are both decisions the numbers have reached; the open band
//     between them is ✓, not yet decided.
//   - row 3 (hook cost): ⚠ when p99 > 250ms or p50 > 50ms, exactly as §10b
//     states it.
//   - row 4 (post_missing): ⚠ when the per-week rate exceeds 1, exactly as
//     §10b states it.
func EmitEnforcementStats(w io.Writer, in EnforcementStats) {
	weeks := in.Since.Hours() / (7 * 24)

	v1 := enforcementRefusalVerdict(in.RefRefused, in.RefRefusedOverridden)
	v2 := enforcementDriftVerdict(in.TreeChangeReported, in.TreeChangeActed)
	v3 := enforcementHookCostVerdict(in.HookCallP50, in.HookCallP99)
	v4 := enforcementPostMissingVerdict(in.PostMissing, weeks)

	glyph := "✓"
	if v1.warn || v2.warn || v3.warn || v4.warn {
		glyph = "⚠"
	}
	fmt.Fprintf(w, "%s enforcement-stats since=%s ref_refused=%d ref_overridden=%d tree_reported=%d tree_acted=%d hook_calls=%d post_missing=%d post_missing_resolved=%d\n",
		glyph, in.Since, in.RefRefused, in.RefRefusedOverridden, in.TreeChangeReported, in.TreeChangeActed,
		in.HookCalls, in.PostMissing, in.PostMissingResolved)

	fmt.Fprintf(w, "%s i1_refusals refused=%d overridden=%d ratio=%s threshold=%s\n",
		rowGlyph(v1.warn), in.RefRefused, in.RefRefusedOverridden, ratioString(in.RefRefusedOverridden, in.RefRefused), v1.threshold)

	fmt.Fprintf(w, "%s drift_acted reported=%d acted=%d ratio=%s threshold=%s\n",
		rowGlyph(v2.warn), in.TreeChangeReported, in.TreeChangeActed, ratioString(in.TreeChangeActed, in.TreeChangeReported), v2.threshold)

	fmt.Fprintf(w, "%s hook_cost p50=%s p99=%s calls=%d top3_locked_bytes=%s threshold=%s\n",
		rowGlyph(v3.warn), fmtMS(in.HookCallP50), fmtMS(in.HookCallP99), in.HookCalls, topLockedBytesString(in.TopLockedBytes), v3.threshold)

	fmt.Fprintf(w, "%s post_missing per_week=%s resolved=%d mean_age_at_resolution=%s threshold=%s\n",
		rowGlyph(v4.warn), rateString(float64(in.PostMissing), weeks), in.PostMissingResolved, fmtMS(in.ResolvedMeanAge), v4.threshold)
}

func rowGlyph(warn bool) string {
	if warn {
		return "⚠"
	}
	return "✓"
}

// enforcementRefusalVerdict — §10b row 1: overrides ≈ refusals means the
// three-shape denylist is wrong, not the operator. No refusals yet is ✓: an
// undefined ratio has nothing to revisit.
func enforcementRefusalVerdict(refused, overridden int) enforcementRowVerdict {
	warn := refused > 0 && overridden >= refused
	return enforcementRowVerdict{warn: warn, threshold: "override≈refusal(ratio≥1.00)"}
}

// enforcementDriftVerdict — §10b row 2: below 1/10 or at/above 1/2 are both
// decisions the numbers have reached (don't build / build); the open band is
// still gathering data and reads ✓. No reports yet is also ✓ — nothing to
// decide.
func enforcementDriftVerdict(reported, acted int) enforcementRowVerdict {
	warn := false
	if reported > 0 {
		ratio := float64(acted) / float64(reported)
		warn = ratio < 1.0/10 || ratio >= 1.0/2
	}
	return enforcementRowVerdict{warn: warn, threshold: "acted/reported<1/10(dont_build)_or>=1/2(build)"}
}

// enforcementHookCostVerdict — §10b row 3, exactly as stated.
func enforcementHookCostVerdict(p50, p99 time.Duration) enforcementRowVerdict {
	warn := p99 > 250*time.Millisecond || p50 > 50*time.Millisecond
	return enforcementRowVerdict{warn: warn, threshold: "p99>250ms_or_p50>50ms"}
}

// enforcementPostMissingVerdict — §10b row 4, exactly as stated: more than
// one per week.
func enforcementPostMissingVerdict(count int, weeks float64) enforcementRowVerdict {
	warn := false
	if weeks > 0 {
		warn = float64(count)/weeks > 1
	}
	return enforcementRowVerdict{warn: warn, threshold: ">1/week"}
}

func ratioString(numerator, denominator int) string {
	if denominator == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", float64(numerator)/float64(denominator))
}

func rateString(count, weeks float64) string {
	if weeks <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", count/weeks)
}

func fmtMS(d time.Duration) string {
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func topLockedBytesString(top []HookTimingTop) string {
	if len(top) == 0 {
		return "none"
	}
	parts := make([]string, len(top))
	for i, t := range top {
		parts[i] = fmt.Sprintf("%s:%d", t.CallID, t.LockedBytes)
	}
	return strings.Join(parts, ",")
}
