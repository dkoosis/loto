package store

import (
	"cmp"
	"context"
	"encoding/json"
	"math"
	"slices"
	"time"
)

// EnforcementStats is what `loto stats` and doctor's first line report:
// enforcement-design.md §10b's four counters, raw, over one window. Render
// decides the thresholds and glyphs; this struct only carries the numbers.
type EnforcementStats struct {
	Since time.Duration

	// Row 1 — is I1 refusing the right things (§10b row 1).
	RefRefused           int
	RefRefusedOverridden int

	// Row 2 — is drift detection reporting anything a holder acts on
	// (§10b row 2). One row per REPORT, one row per ACT, matching
	// EventTreeChangeReported / EventTreeChangeActed's own counting rule.
	TreeChangeReported int
	TreeChangeActed    int

	// Row 3 — what the hook costs (§10b row 3). P50/P99 are computed over
	// each call's own pre_ms + post_ms total; Calls is how many distinct
	// calls contributed at least one hook_timing row in the window.
	// TopLockedBytes is the top 3 calls by locked_bytes, deterministically
	// sorted (bytes descending, call_id ascending on a tie).
	HookCallP50    time.Duration
	HookCallP99    time.Duration
	HookCalls      int
	TopLockedBytes []HookTimingTop

	// Row 4 — how often a post-hook goes missing on a live session
	// (§10b row 4). PostMissing is calls flagged over the window;
	// PostMissingResolved is how many of those resolved (posted late, or
	// their owner died) within the same window; ResolvedMeanAge is the mean
	// of AgeMS across resolved rows only.
	PostMissing         int
	PostMissingResolved int
	ResolvedMeanAge     time.Duration
}

// HookTimingTop is one call's slot in the top-3-by-locked_bytes list.
type HookTimingTop struct {
	CallID      string
	LockedBytes int64
}

// hookTimingPayload mirrors cmd_hook.go's hookTimingDetail. Duplicated rather
// than imported: internal/store does not depend on internal/cli, and this is
// the read side of a JSON wire shape, not shared Go logic.
type hookTimingPayload struct {
	CallID      string `json:"call_id"`
	PreMs       int64  `json:"pre_ms,omitempty"`
	PostMs      int64  `json:"post_ms,omitempty"`
	LockedBytes int64  `json:"locked_bytes"`
}

// ReadEnforcementStats aggregates enforcement-design.md §10b's four counters
// over the last `since`, reading only the events table (no new store, per
// this bead's Story). Bounded by the same events retention ReadGateStats
// documents — a window wider than retention reports what the audit trail
// still holds.
//
// Split into one helper per row group (readEnforcementCounts,
// readPostMissingResolved, readHookTiming) rather than one long function:
// each reads a different shape out of events.detail, and keeping them apart
// is what keeps each one readable.
func (s *Store) ReadEnforcementStats(ctx context.Context, since time.Duration) (EnforcementStats, error) {
	out := EnforcementStats{Since: since}
	cutoff := time.Now().Add(-since).UnixNano()

	if err := s.readEnforcementCounts(ctx, cutoff, &out); err != nil {
		return out, err
	}
	resolved, meanAge, err := s.readPostMissingResolved(ctx, cutoff)
	if err != nil {
		return out, err
	}
	out.PostMissingResolved = resolved
	out.ResolvedMeanAge = meanAge

	calls, p50, p99, top, err := s.readHookTiming(ctx, cutoff)
	if err != nil {
		return out, err
	}
	out.HookCalls = calls
	out.HookCallP50 = p50
	out.HookCallP99 = p99
	out.TopLockedBytes = top

	return out, nil
}

// readEnforcementCounts fills rows 1, 2 and post_missing's raw count: five
// kinds, one grouped count, no Detail parsing needed.
func (s *Store) readEnforcementCounts(ctx context.Context, cutoff int64, out *EnforcementStats) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_kind, COUNT(*) FROM events
		  WHERE created_at >= ? AND event_kind IN (?, ?, ?, ?, ?)
		  GROUP BY event_kind`,
		cutoff, EventRefRefused, EventRefRefusedOverridden,
		EventTreeChangeReported, EventTreeChangeActed, EventPostMissing)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return err
		}
		switch kind {
		case EventRefRefused:
			out.RefRefused = n
		case EventRefRefusedOverridden:
			out.RefRefusedOverridden = n
		case EventTreeChangeReported:
			out.TreeChangeReported = n
		case EventTreeChangeActed:
			out.TreeChangeActed = n
		case EventPostMissing:
			out.PostMissing = n
		}
	}
	return rows.Err()
}

// readPostMissingResolved counts post_missing_resolved rows and their mean
// AgeMS — the one row group the grouped count above can't carry, since the
// age lives in Detail, not in a column.
func (s *Store) readPostMissingResolved(ctx context.Context, cutoff int64) (count int, meanAge time.Duration, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT detail FROM events WHERE created_at >= ? AND event_kind = ?`,
		cutoff, EventPostMissingResolved)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	var ageSum int64
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			return 0, 0, err
		}
		count++
		var d postMissingDetail
		if json.Unmarshal([]byte(detail), &d) == nil {
			ageSum += d.AgeMS
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if count > 0 {
		meanAge = time.Duration(ageSum/int64(count)) * time.Millisecond
	}
	return count, meanAge, nil
}

// readHookTiming aggregates hook_timing rows per call_id from Detail (the
// kind writes no Target — enforcement-design.md §10b row 3, event_kinds.go),
// then reduces that to the p50/p99 of each call's own total and the top 3 by
// locked_bytes.
func (s *Store) readHookTiming(ctx context.Context, cutoff int64) (calls int, p50, p99 time.Duration, top []HookTimingTop, err error) {
	byCall, err := s.readHookTimingPerCall(ctx, cutoff)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	if len(byCall) == 0 {
		return 0, 0, 0, nil, nil
	}

	totals := make([]int64, 0, len(byCall))
	top = make([]HookTimingTop, 0, len(byCall))
	for callID, a := range byCall {
		totals = append(totals, a.totalMS)
		top = append(top, HookTimingTop{CallID: callID, LockedBytes: a.lockedBytes})
	}
	slices.Sort(totals)
	p50 = time.Duration(percentileMS(totals, 50)) * time.Millisecond
	p99 = time.Duration(percentileMS(totals, 99)) * time.Millisecond

	slices.SortFunc(top, func(a, b HookTimingTop) int {
		return cmp.Or(cmp.Compare(b.LockedBytes, a.LockedBytes), cmp.Compare(a.CallID, b.CallID))
	})
	if len(top) > 3 {
		top = top[:3]
	}
	return len(byCall), p50, p99, top, nil
}

// hookCallAgg is one call_id's reduction across its pre and post hook_timing
// rows: the phases' combined cost and the largest locked_bytes either half
// reported.
type hookCallAgg struct {
	totalMS     int64
	lockedBytes int64
}

func (s *Store) readHookTimingPerCall(ctx context.Context, cutoff int64) (map[string]*hookCallAgg, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT reason, detail FROM events WHERE created_at >= ? AND event_kind = ?`,
		cutoff, EventHookTiming)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byCall := map[string]*hookCallAgg{}
	for rows.Next() {
		var reason, detail string
		if err := rows.Scan(&reason, &detail); err != nil {
			return nil, err
		}
		var d hookTimingPayload
		if json.Unmarshal([]byte(detail), &d) != nil || d.CallID == "" {
			continue
		}
		a := byCall[d.CallID]
		if a == nil {
			a = &hookCallAgg{}
			byCall[d.CallID] = a
		}
		switch reason {
		case "pre":
			a.totalMS += d.PreMs
		case "post":
			a.totalMS += d.PostMs
		}
		a.lockedBytes = max(a.lockedBytes, d.LockedBytes)
	}
	return byCall, rows.Err()
}

// percentileMS returns the nearest-rank percentile p (0-100) over sorted,
// ascending, non-empty. Nearest-rank rather than interpolated: §10b's
// thresholds ("p99 > 250ms") are decision points, not statistics that need
// smoothing between the two nearest samples.
func percentileMS(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	idx = max(idx, 0)
	idx = min(idx, len(sorted)-1)
	return sorted[idx]
}
