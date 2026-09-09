package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

// tcSHA3 is the third digest the two-transition case needs: d0 -> d1 -> d2.
const (
	tcTreeCall41 = "call-41"
	tcSHA3       = "3333333333333333333333333333333333333333"
)

// obsFor is one locked-path observation of tcHookPath at epoch 1, held by A.
// stat tracks the digest so a change is a change by both tests, the way a real
// write moves size and mtime together.
func obsFor(digest string) HookPathState {
	return HookPathState{
		Path: tcHookPath, Locked: true, Epoch: 1, Holder: tcOwnerA,
		Digest: digest, Stat: "stat-" + digest,
	}
}

// preAs opens a call for an arbitrary owner. mustPre is owner A only.
func preAs(t *testing.T, s *Store, owner domain.AgentUUID, callID string, at time.Time, obs ...HookPathState) {
	t.Helper()
	ok, err := s.RecordCallPre(context.Background(), HookCall{
		CallID: callID, OwnerUUID: owner, SessionUUID: domain.SessionUUID("sess-" + owner),
		ToolName: "Bash", TPre: at,
	}, obs)
	if err != nil {
		t.Fatalf("pre %s: %v", callID, err)
	}
	if !ok {
		t.Fatalf("pre %s: want recorded, got no-op", callID)
	}
}

func postAt(t *testing.T, s *Store, callID string, at time.Time, obs ...HookPathState) PostOutcome {
	t.Helper()
	out, err := s.RecordCallPost(context.Background(), callID, at, obs)
	if err != nil {
		t.Fatalf("post %s: %v", callID, err)
	}
	return out
}

// spannerOwners is the set of owners named on an event, for comparison by eye.
func spannerOwners(ev TreeEvent) []string {
	out := make([]string, 0, len(ev.Spanners))
	for i := range ev.Spanners {
		out = append(out, string(ev.Spanners[i].Owner))
	}
	return out
}

func eventAtSeq(t *testing.T, s *Store, path string, seq int64, observer domain.AgentUUID) TreeEvent {
	t.Helper()
	evs, err := s.TreeEventsFor(context.Background(), path)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for i := range evs {
		if evs[i].Seq == seq && evs[i].Observer == observer {
			return evs[i]
		}
	}
	t.Fatalf("no event at seq %d observed by %s: %+v", seq, observer, evs)
	return TreeEvent{}
}

func addresseesOf(t *testing.T, s *Store, eventID string) []string {
	t.Helper()
	reports, err := s.UndeliveredReports(context.Background(), "")
	if err != nil {
		t.Fatalf("read reports: %v", err)
	}
	var out []string
	for i := range reports {
		if reports[i].Event.EventID == eventID {
			out = append(out, string(reports[i].Addressee))
		}
	}
	return out
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestMigrate_AddsTreeEventTables(t *testing.T) {
	s := mustOpen(t)
	for _, table := range []string{"path_observed", "tree_events", "tree_event_spanners", "tree_reports"} {
		var n int
		if err := s.db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("want table %s present, got count=%d", table, n)
		}
	}
}

// §10a test 1, reporting half. A holds f and runs a call that writes nothing
// (the spec's `sleep`); B's call writes f inside A's window. B's event names A
// as a spanner, A's own event names B, and both are reported to both.
//
// The two events sit on ONE transition: B's write is one physical change, and
// A's post observes the state B's post already numbered.
func TestTreeEvent_HolderNoOpOverlappingAPeerWrite(t *testing.T) {
	s := mustOpen(t)
	now := time.Now()

	preAs(t, s, tcOwnerA, "a-sleep", now, obsFor(tcSHA1))
	preAs(t, s, tcOwnerB, "b-write", now.Add(time.Millisecond), obsFor(tcSHA1))

	// B posts first, having written f.
	outB := postAt(t, s, "b-write", now.Add(time.Second), obsFor(tcSHA2))
	if len(outB.Events) != 1 {
		t.Fatalf("want one event at B's post, got %d", len(outB.Events))
	}
	evB := outB.Events[0]
	if evB.Seq != 1 {
		t.Errorf("B's transition wants seq 1, got %d", evB.Seq)
	}
	if got := spannerOwners(evB); !sameStrings(got, []string{tcOwnerA}) {
		t.Errorf("B's event must name A as a spanner, got %v", got)
	}
	if evB.Rule != TreeRuleRow6 {
		t.Errorf("h_pre=A, observer=B, contested wants row6, got %s", evB.Rule)
	}

	// A's own post lands after, observing the state B's post already numbered.
	outA := postAt(t, s, "a-sleep", now.Add(2*time.Second), obsFor(tcSHA2))
	if len(outA.Events) != 1 {
		t.Fatalf("want one event at A's post, got %d", len(outA.Events))
	}
	evA := outA.Events[0]
	if evA.Seq != evB.Seq {
		t.Errorf("one physical change wants one transition number: B=%d A=%d", evB.Seq, evA.Seq)
	}
	if got := spannerOwners(evA); !sameStrings(got, []string{tcOwnerB}) {
		t.Errorf("A's event must name B, got %v", got)
	}
	if evA.Rule != TreeRuleRow4 {
		t.Errorf("h_pre=observer=A, contested, undeclared wants row4, got %s", evA.Rule)
	}

	for _, ev := range []TreeEvent{evA, evB} {
		got := addresseesOf(t, s, ev.EventID)
		if !sameStrings(got, []string{tcOwnerA, tcOwnerB}) {
			t.Errorf("event %s (%s) must report to both owners, got %v", ev.EventID, ev.Rule, got)
		}
	}
}

// §10a test 2. A's post landed with seq_post BEFORE B's transition was
// assigned — A observed no change, so its seq_post stayed at seq_pre. B's
// event names no spanner: A is out by ordering, and by ordering alone.
func TestTreeEvent_SpannerExoneratedByItsPost(t *testing.T) {
	s := mustOpen(t)
	now := time.Now()

	preAs(t, s, tcOwnerA, "a-noop", now, obsFor(tcSHA1))
	preAs(t, s, tcOwnerB, "b-write", now.Add(time.Millisecond), obsFor(tcSHA1))

	// A posts having changed nothing: no event, and seq_post == seq_pre == 0.
	outA := postAt(t, s, "a-noop", now.Add(time.Second), obsFor(tcSHA1))
	if len(outA.Events) != 0 {
		t.Fatalf("an unchanged path files no event, got %+v", outA.Events)
	}

	outB := postAt(t, s, "b-write", now.Add(2*time.Second), obsFor(tcSHA2))
	if len(outB.Events) != 1 {
		t.Fatalf("want one event at B's post, got %d", len(outB.Events))
	}
	if got := spannerOwners(outB.Events[0]); len(got) != 0 {
		t.Errorf("A posted before the transition; want no spanner, got %v", got)
	}
	if outB.Events[0].Rule != TreeRuleRow5 {
		t.Errorf("h_pre=A, observer=B, uncontested wants row5, got %s", outB.Events[0].Rule)
	}
	// Row 5 reports to the holder and the observer, and to nobody else.
	if got := addresseesOf(t, s, outB.Events[0].EventID); !sameStrings(got, []string{tcOwnerA, tcOwnerB}) {
		t.Errorf("row5 addresses holder and observer, got %v", got)
	}
}

// §10a test 12, contention half. B's Bash call spans two transitions of f by
// A. Both of A's events name B, and B's later post at seq 2 changes neither —
// an event is final at filing, so a verdict cannot be rolled back onto it.
func TestTreeEvent_OneCallSpanningTwoTransitions(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	preAs(t, s, tcOwnerB, "b-long", now, obsFor(tcSHA1))

	preAs(t, s, tcOwnerA, "a-1", now.Add(time.Millisecond), obsFor(tcSHA1))
	postAt(t, s, "a-1", now.Add(time.Second), obsFor(tcSHA2))
	preAs(t, s, tcOwnerA, "a-2", now.Add(2*time.Second), obsFor(tcSHA2))
	postAt(t, s, "a-2", now.Add(3*time.Second), obsFor(tcSHA3))

	for _, seq := range []int64{1, 2} {
		ev := eventAtSeq(t, s, tcHookPath, seq, tcOwnerA)
		if got := spannerOwners(ev); !sameStrings(got, []string{tcOwnerB}) {
			t.Errorf("A's event at seq %d must name B, got %v", seq, got)
		}
		if ev.Rule != TreeRuleRow4 {
			t.Errorf("A's event at seq %d wants row4, got %s", seq, ev.Rule)
		}
	}

	before, err := s.TreeEventsFor(ctx, tcHookPath)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	// B posts at seq 2 having written nothing: it observes the state A's
	// second call already numbered, so it files its own event against
	// transition 2 and touches neither of A's.
	postAt(t, s, "b-long", now.Add(4*time.Second), obsFor(tcSHA3))
	after, err := s.TreeEventsFor(ctx, tcHookPath)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for i := range before {
		for j := range after {
			if before[i].EventID != after[j].EventID {
				continue
			}
			if before[i].Rule != after[j].Rule || before[i].Seq != after[j].Seq ||
				len(before[i].Spanners) != len(after[j].Spanners) {
				t.Errorf("B's post changed an existing event: %+v -> %+v", before[i], after[j])
			}
		}
	}
	if got := seqOf(t, s, tcHookPath); got != 2 {
		t.Errorf("B wrote nothing; the sequence must stay at 2, got %d", got)
	}
}

func seqOf(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	n, err := s.PathSeq(context.Background(), path, 1)
	if err != nil {
		t.Fatalf("read seq: %v", err)
	}
	return n
}

// §10a test 14 (R7). A's call 41 never posts and T_report has passed, so it is
// post_missing. B's change to a path 41 recorded still names call 41 as a
// spanner: age never exonerates, because a command that has not posted may
// still be running and may write next.
func TestTreeEvent_PostMissingCallStillSpans(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	preAs(t, s, tcOwnerA, tcTreeCall41, now.Add(-time.Hour), obsFor(tcSHA1))
	if n, err := s.MarkPostMissing(ctx, now, 10*time.Minute); err != nil || n != 1 {
		t.Fatalf("post_missing sweep: n=%d err=%v", n, err)
	}

	preAs(t, s, tcOwnerB, "b-write", now, obsFor(tcSHA1))
	out := postAt(t, s, "b-write", now.Add(time.Second), obsFor(tcSHA2))
	if len(out.Events) != 1 {
		t.Fatalf("want one event, got %d", len(out.Events))
	}
	ev := out.Events[0]
	if len(ev.Spanners) != 1 || ev.Spanners[0].CallID != tcTreeCall41 {
		t.Fatalf("want call 41 named as the spanner, got %+v", ev.Spanners)
	}
	if ev.Rule != TreeRuleRow6 {
		t.Errorf("a post_missing spanner makes the event contested; want row6, got %s", ev.Rule)
	}
}

// §6's fourth variant: "A is dead. Not in flight, so nothing spans." A call
// ended by the liveness probe has ended, and a dead process cannot write next.
func TestTreeEvent_DeadOwnersCallSpansNothing(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	preAs(t, s, tcOwnerA, "a-dead", now, obsFor(tcSHA1))
	if n, err := s.MarkDeadOwnerCalls(ctx, now, func(domain.SessionUUID) bool { return true }); err != nil || n != 1 {
		t.Fatalf("dead-owner sweep: n=%d err=%v", n, err)
	}

	preAs(t, s, tcOwnerB, "b-write", now, obsFor(tcSHA1))
	out := postAt(t, s, "b-write", now.Add(time.Second), obsFor(tcSHA2))
	if len(out.Events) != 1 {
		t.Fatalf("want one event, got %d", len(out.Events))
	}
	if got := spannerOwners(out.Events[0]); len(got) != 0 {
		t.Errorf("a dead owner's call spans nothing, got %v", got)
	}
	if out.Events[0].Rule != TreeRuleRow5 {
		t.Errorf("uncontested peer-held change wants row5, got %s", out.Events[0].Rule)
	}
}

// Row 1: the epoch moved during the call. The report names the observer, the
// holder the call recorded, and the holder now — nobody else can tell the
// addressee that the authorization it was writing under is gone.
func TestTreeEvent_EpochMovedDuringTheCallIsRow1(t *testing.T) {
	s := mustOpen(t)
	now := time.Now()

	preAs(t, s, tcOwnerB, "b-1", now, obsFor(tcSHA1))
	out := postAt(t, s, "b-1", now.Add(time.Second), HookPathState{
		Path: tcHookPath, Locked: true, Epoch: 2, Holder: "owner-c",
		Digest: tcSHA2, Stat: "stat-" + tcSHA2,
	})
	if len(out.Events) != 1 {
		t.Fatalf("want one event, got %d", len(out.Events))
	}
	if out.Events[0].Rule != TreeRuleRow1 {
		t.Fatalf("a moved epoch wants row1, got %s", out.Events[0].Rule)
	}
	got := addresseesOf(t, s, out.Events[0].EventID)
	if !sameStrings(got, []string{tcOwnerA, tcOwnerB, "owner-c"}) {
		t.Errorf("row1 tells the observer, the old holder and the current one, got %v", got)
	}
}

// A dirtied unlocked path with nobody else in flight is row 8: an event is
// filed, and no report is written — the row's only output is `L`'s
// compare-and-set, which this bead does not build.
func TestTreeEvent_UnlockedUncontestedFilesAnEventAndNoReport(t *testing.T) {
	s := mustOpen(t)
	now := time.Now()

	unlocked := HookPathState{Path: tcHookPath2, Digest: tcSHA1, Stat: "stat-1"}
	preAs(t, s, tcOwnerA, "a-1", now, unlocked)
	unlocked.Digest, unlocked.Stat = tcSHA2, "stat-2"
	out := postAt(t, s, "a-1", now.Add(time.Second), unlocked)
	if len(out.Events) != 1 || out.Events[0].Rule != TreeRuleRow8 {
		t.Fatalf("want one row8 event, got %+v", out.Events)
	}
	if got := addresseesOf(t, s, out.Events[0].EventID); len(got) != 0 {
		t.Errorf("row8 writes no report, got %v", got)
	}
}

// Drift (I3 step 2), at the store layer: the first pass over a locked path
// SEEDS observed(f) and reports nothing; a later pass whose state differs
// files one event and moves observed(f), so a third pass is silent.
func TestRecordDrift_SeedsThenReportsOnce(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	out, err := s.RecordDrift(ctx, tcOwnerB, now, []HookPathState{obsFor(tcSHA1)})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(out.Drifted) != 0 || len(out.Seeded) != 1 {
		t.Fatalf("the first observation seeds, it does not drift: %+v", out)
	}

	out, err = s.RecordDrift(ctx, tcOwnerB, now.Add(time.Second), []HookPathState{obsFor(tcSHA2)})
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if len(out.Drifted) != 1 {
		t.Fatalf("a changed locked path drifts: %+v", out)
	}
	ev := eventAtSeq(t, s, tcHookPath, 1, tcOwnerB)
	if ev.Rule != TreeRuleDrift || ev.DigestPre != tcSHA1 || ev.DigestPost != tcSHA2 {
		t.Errorf("want a drift event %s -> %s, got %+v", tcSHA1, tcSHA2, ev)
	}
	if got := addresseesOf(t, s, ev.EventID); !sameStrings(got, []string{tcOwnerA, tcOwnerB}) {
		t.Errorf("drift reports to the holder (and the detector), got %v", got)
	}

	out, err = s.RecordDrift(ctx, tcOwnerB, now.Add(2*time.Second), []HookPathState{obsFor(tcSHA2)})
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if len(out.Drifted) != 0 {
		t.Errorf("observed(f) moved; the same drift must not report twice: %+v", out)
	}
}

// §10b row 2's pair. A report is one tree_change_reported row per addressee;
// the addressee's own next call putting the reported digest back, inside
// TreeActedWindow OF THE DELIVERY, is one tree_change_acted row.
func TestTreeChange_ReportedThenActedAreBothCounted(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	preAs(t, s, tcOwnerB, "b-write", now, obsFor(tcSHA1))
	out := postAt(t, s, "b-write", now.Add(time.Second), obsFor(tcSHA2))
	if len(out.Events) != 1 {
		t.Fatalf("want one event, got %d", len(out.Events))
	}
	if got := countEventKind(t, s, EventTreeChangeReported); got != 2 {
		t.Errorf("row5 reports to two owners, so two reported rows; got %d", got)
	}

	// A is handed the report, then puts the reported digest back.
	if _, err := s.DeliverReports(ctx, tcOwnerA, now.Add(2*time.Second)); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	preAs(t, s, tcOwnerA, "a-fix", now.Add(3*time.Second), obsFor(tcSHA2))
	fixed := postAt(t, s, "a-fix", now.Add(4*time.Second), obsFor(tcSHA1))
	if !sameStrings(fixed.Acted, []string{tcHookPath}) {
		t.Errorf("A rewrote f back to the reported digest; want it counted, got %v", fixed.Acted)
	}
	if got := countEventKind(t, s, EventTreeChangeActed); got != 1 {
		t.Errorf("want one acted row, got %d", got)
	}
}

// The acted window's two edges, and its precondition. Undelivered is never
// acted on — a report nobody read moved nobody — and the ten minutes run from
// the DELIVERY, not from when the report was filed.
func TestTreeChange_ActedKeysOffDeliveryNotFiling(t *testing.T) {
	filed := time.Now()

	t.Run("undelivered is never acted on", func(t *testing.T) {
		s := mustOpen(t)
		seedOneReport(t, s, filed)
		out := rewriteBackAt(t, s, "a-fix", filed.Add(time.Minute))
		if len(out.Acted) != 0 {
			t.Errorf("the report was never handed over; want no acted, got %v", out.Acted)
		}
		if got := countEventKind(t, s, EventTreeChangeActed); got != 0 {
			t.Errorf("want no acted row, got %d", got)
		}
	})

	t.Run("delivered late, rewritten inside the window of the delivery", func(t *testing.T) {
		s := mustOpen(t)
		seedOneReport(t, s, filed)
		// Delivery an hour after filing: keyed off created_at this would be
		// outside the window and count nothing.
		late := filed.Add(time.Hour)
		if _, err := s.DeliverReports(context.Background(), tcOwnerA, late); err != nil {
			t.Fatalf("deliver: %v", err)
		}
		out := rewriteBackAt(t, s, "a-fix", late.Add(time.Minute))
		if !sameStrings(out.Acted, []string{tcHookPath}) {
			t.Errorf("a rewrite one minute after delivery is acting on it, got %v", out.Acted)
		}
	})

	t.Run("delivered, rewritten past the window", func(t *testing.T) {
		s := mustOpen(t)
		seedOneReport(t, s, filed)
		if _, err := s.DeliverReports(context.Background(), tcOwnerA, filed.Add(time.Second)); err != nil {
			t.Fatalf("deliver: %v", err)
		}
		out := rewriteBackAt(t, s, "a-fix", filed.Add(TreeActedWindow+time.Hour))
		if len(out.Acted) != 0 {
			t.Errorf("a rewrite past the window is a coincidence, not an act: %v", out.Acted)
		}
		if got := countEventKind(t, s, EventTreeChangeActed); got != 0 {
			t.Errorf("want no acted row, got %d", got)
		}
	})
}

// §10b row 2's denominator has one row per report, so its numerator must too.
// A session that keeps working while the file stays reverted files one acted
// row, not one per tool call.
func TestTreeChange_ActedIsCountedOncePerReport(t *testing.T) {
	s := mustOpen(t)
	now := time.Now()
	seedOneReport(t, s, now)
	if _, err := s.DeliverReports(context.Background(), tcOwnerA, now.Add(time.Second)); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	first := rewriteBackAt(t, s, "a-fix-1", now.Add(2*time.Second))
	if len(first.Acted) != 1 {
		t.Fatalf("the first rewrite counts: %v", first.Acted)
	}
	for i, id := range []string{"a-still-1", "a-still-2"} {
		out := rewriteBackAt(t, s, id, now.Add(time.Duration(3+i)*time.Second))
		if len(out.Acted) != 0 {
			t.Errorf("%s: the same report was counted a second time: %v", id, out.Acted)
		}
	}
	if got := countEventKind(t, s, EventTreeChangeActed); got != 1 {
		t.Errorf("want exactly one acted row per report, got %d", got)
	}
}

// seedOneReport files one row5 event over tcHookPath by a peer's write, which
// leaves A a report saying the path went tcSHA1 -> tcSHA2.
func seedOneReport(t *testing.T, s *Store, at time.Time) {
	t.Helper()
	preAs(t, s, tcOwnerB, "b-write", at, obsFor(tcSHA1))
	postAt(t, s, "b-write", at.Add(time.Millisecond), obsFor(tcSHA2))
}

// rewriteBackAt is one call by A that puts tcSHA1 back on disk.
func rewriteBackAt(t *testing.T, s *Store, callID string, at time.Time) PostOutcome {
	t.Helper()
	preAs(t, s, tcOwnerA, callID, at, obsFor(tcSHA2))
	return postAt(t, s, callID, at.Add(time.Millisecond), obsFor(tcSHA1))
}

// A change some in-flight call already recorded is DEFERRED, not filed: that
// call's post files it, with the spanning calls named. Filing here as well
// would give one physical change two events and two report sets, which is a
// straight skew of §10b's reported count — and a drift event whose own note
// says "no call covering it" while naming that very call as a spanner.
func TestRecordDrift_DefersToAnInFlightCallThatRecordedThePath(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	// A's earlier call seeds observed(f) at d0.
	if _, err := s.RecordDrift(ctx, tcOwnerA, now, []HookPathState{obsFor(tcSHA1)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// B opens a call over the path, then writes.
	preAs(t, s, tcOwnerB, "b-write", now.Add(time.Second), obsFor(tcSHA1))

	// A's pre lands before B posts: the file already reads d1.
	out, err := s.RecordDrift(ctx, tcOwnerA, now.Add(2*time.Second), []HookPathState{obsFor(tcSHA2)})
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if len(out.Drifted) != 0 || !sameStrings(out.Deferred, []string{tcHookPath}) {
		t.Fatalf("want the change deferred to B's post, got %+v", out)
	}
	if got := len(treeEventsOf(t, s, tcHookPath)); got != 0 {
		t.Fatalf("drift filed an event B's post will file too: %d", got)
	}

	// B posts. One event, one report set, and B's own call is not its spanner.
	postAt(t, s, "b-write", now.Add(3*time.Second), obsFor(tcSHA2))
	evs := treeEventsOf(t, s, tcHookPath)
	if len(evs) != 1 {
		t.Fatalf("one physical change wants exactly one event, got %d: %+v", len(evs), evs)
	}
	if evs[0].Rule != TreeRuleRow5 {
		t.Errorf("want B's post to file row5, got %s", evs[0].Rule)
	}
	if got := addresseesOf(t, s, evs[0].EventID); !sameStrings(got, []string{tcOwnerA, tcOwnerB}) {
		t.Errorf("want exactly one report set, got %v", got)
	}

	// The change is not lost while deferred: B's post moved observed(f), so
	// A's next pre is silent rather than filing the drift late.
	out, err = s.RecordDrift(ctx, tcOwnerA, now.Add(4*time.Second), []HookPathState{obsFor(tcSHA2)})
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if len(out.Drifted) != 0 || len(out.Deferred) != 0 {
		t.Errorf("B's post settled it; A's next pre has nothing to file: %+v", out)
	}
}

// Deferring writes nothing, observed(f) included, so a change under a call
// whose owner then DIES is filed as drift by the next pre rather than lost.
func TestRecordDrift_FilesAfterTheDeferredCallsOwnerDies(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := s.RecordDrift(ctx, tcOwnerA, now, []HookPathState{obsFor(tcSHA1)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	preAs(t, s, tcOwnerB, "b-gone", now.Add(time.Second), obsFor(tcSHA1))
	out, err := s.RecordDrift(ctx, tcOwnerA, now.Add(2*time.Second), []HookPathState{obsFor(tcSHA2)})
	if err != nil || len(out.Deferred) != 1 {
		t.Fatalf("want deferred: %+v err=%v", out, err)
	}

	if n, err := s.MarkDeadOwnerCalls(ctx, now, func(domain.SessionUUID) bool { return true }); err != nil || n != 1 {
		t.Fatalf("dead-owner sweep: n=%d err=%v", n, err)
	}
	out, err = s.RecordDrift(ctx, tcOwnerA, now.Add(3*time.Second), []HookPathState{obsFor(tcSHA2)})
	if err != nil {
		t.Fatalf("after death: %v", err)
	}
	if !sameStrings(out.Drifted, []string{tcHookPath}) {
		t.Fatalf("a change under a dead owner's call is drift, not lost: %+v", out)
	}
	if evs := treeEventsOf(t, s, tcHookPath); len(evs) != 1 || evs[0].Rule != TreeRuleDrift {
		t.Errorf("want one drift event, got %+v", evs)
	}
}

// An unchanged locked path is in NEITHER slice: Seeded means seeded.
func TestRecordDrift_UnchangedPathIsNeitherSeededNorDrifted(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	out, err := s.RecordDrift(ctx, tcOwnerA, now, []HookPathState{obsFor(tcSHA1)})
	if err != nil || !sameStrings(out.Seeded, []string{tcHookPath}) {
		t.Fatalf("the first pass seeds: %+v err=%v", out, err)
	}
	out, err = s.RecordDrift(ctx, tcOwnerA, now.Add(time.Second), []HookPathState{obsFor(tcSHA1)})
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(out.Seeded) != 0 || len(out.Drifted) != 0 || len(out.Deferred) != 0 {
		t.Errorf("an already-observed, unchanged path belongs in no slice: %+v", out)
	}
}

func treeEventsOf(t *testing.T, s *Store, path string) []TreeEvent {
	t.Helper()
	evs, err := s.TreeEventsFor(context.Background(), path)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return evs
}

// Retention must never silently swallow a report nobody has read. The row cap
// is applied only to events nothing is waiting on, so an idle session's
// reports survive any number of later changes — otherwise `reported` counts
// them and `acted` never can, and §10b's ratio reads as "holders ignore
// reports" when nobody was ever handed one.
func TestDropOldTreeEvents_NeverEvictsAnUndeliveredReport(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	// Five reports addressed to A, left unread, filed FIRST — the oldest rows,
	// and the first casualties of a cap that did not know about them.
	for i := range 5 {
		seedReportOn(t, s, "waiting-"+itoa(i), now.Add(time.Duration(i)*time.Millisecond))
	}
	if got := len(mustUndelivered(t, s, tcOwnerA)); got != 5 {
		t.Fatalf("want 5 reports waiting for A, got %d", got)
	}

	// Then 1200 events on other paths, every one of them read.
	busy := now.Add(time.Minute)
	for i := range 1200 {
		seedReportOn(t, s, "busy-"+itoa(i), busy.Add(time.Duration(i)*time.Millisecond))
	}
	// Every event so far addresses both the holder and the observer, so both
	// have to read for the event to stop being exempt.
	for _, who := range []domain.AgentUUID{tcOwnerA, tcOwnerB} {
		if _, err := s.DeliverReports(ctx, who, busy.Add(time.Hour)); err != nil {
			t.Fatalf("deliver to %s: %v", who, err)
		}
	}
	// A's five are read now too, so re-file five fresh unread ones and only
	// then let the cap run — the case is "unread rows beyond the cap".
	for i := range 5 {
		seedReportOn(t, s, "waiting-"+itoa(i), busy.Add(2*time.Hour+time.Duration(i)*time.Millisecond))
	}
	before := len(mustUndelivered(t, s, tcOwnerA))
	if before != 5 {
		t.Fatalf("want 5 unread reports before the sweep, got %d", before)
	}

	if _, err := s.DropOldTreeEvents(ctx, busy.Add(3*time.Hour)); err != nil {
		t.Fatalf("retention: %v", err)
	}
	if got := len(mustUndelivered(t, s, tcOwnerA)); got != 5 {
		t.Fatalf("the cap evicted an unread report: 5 -> %d", got)
	}
	if got := countEventKind(t, s, EventTreeReportDropped); got != 0 {
		t.Errorf("nothing aged out; want no dropped rows, got %d", got)
	}
	// The cap did run: the read events are bounded.
	var events int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tree_events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events > treeEventsRetentionMax+before {
		t.Errorf("the cap did not run: %d events left", events)
	}
}

// An undelivered report expires by AGE, and leaves a countable row when it
// does — a silent deletion is what skews the ratio.
func TestDropOldTreeEvents_AgedOutUndeliveredLeavesADroppedRow(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	old := time.Now().Add(-treeReportUndeliveredAge - time.Hour)

	seedReportOn(t, s, "stale", old)
	if got := len(mustUndelivered(t, s, tcOwnerA)); got != 1 {
		t.Fatalf("want one report waiting, got %d", got)
	}
	if _, err := s.DropOldTreeEvents(ctx, time.Now()); err != nil {
		t.Fatalf("retention: %v", err)
	}
	if got := len(mustUndelivered(t, s, tcOwnerA)); got != 0 {
		t.Errorf("an aged-out undelivered report is dropped, got %d left", got)
	}
	// Two addressees, so two rows aged out together.
	if got := countEventKind(t, s, EventTreeReportDropped); got != 2 {
		t.Errorf("want the loss counted, got %d dropped rows", got)
	}
}

// seedReportOn files one row5 event over a named path, leaving A a report.
func seedReportOn(t *testing.T, s *Store, path string, at time.Time) {
	t.Helper()
	obs := func(d string) HookPathState {
		return HookPathState{Path: path, Locked: true, Epoch: 1, Holder: tcOwnerA, Digest: d, Stat: "stat-" + d}
	}
	callID := path + "@" + at.Format(time.RFC3339Nano)
	preAs(t, s, tcOwnerB, callID, at, obs(tcSHA1))
	postAt(t, s, callID, at.Add(time.Microsecond), obs(tcSHA2))
}

func mustUndelivered(t *testing.T, s *Store, who domain.AgentUUID) []TreeReport {
	t.Helper()
	out, err := s.UndeliveredReports(context.Background(), who)
	if err != nil {
		t.Fatalf("read undelivered: %v", err)
	}
	return out
}

// itoa keeps the fixture path names stdlib-plain without pulling strconv in
// for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func countEventKind(t *testing.T, s *Store, kind string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM events WHERE event_kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", kind, err)
	}
	return n
}

// Delivery is a write, and it happens once. A second call by the same owner
// gets nothing, and the report leaves the undelivered list.
func TestDeliverReports_HandsEachReportOverExactlyOnce(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	preAs(t, s, tcOwnerB, "b-write", now, obsFor(tcSHA1))
	postAt(t, s, "b-write", now.Add(time.Second), obsFor(tcSHA2))

	pending, err := s.UndeliveredReports(ctx, tcOwnerA)
	if err != nil || len(pending) != 1 {
		t.Fatalf("want one report waiting for A: n=%d err=%v", len(pending), err)
	}
	first, err := s.DeliverReports(ctx, tcOwnerA, now.Add(2*time.Second))
	if err != nil || len(first) != 1 {
		t.Fatalf("want one delivered: n=%d err=%v", len(first), err)
	}
	second, err := s.DeliverReports(ctx, tcOwnerA, now.Add(3*time.Second))
	if err != nil || len(second) != 0 {
		t.Fatalf("a delivered report is not delivered twice: n=%d err=%v", len(second), err)
	}
	left, err := s.UndeliveredReports(ctx, tcOwnerA)
	if err != nil || len(left) != 0 {
		t.Fatalf("delivery must remove it from the undelivered list: n=%d err=%v", len(left), err)
	}
	// B's copy of the same report is untouched: delivery is per addressee.
	bs, err := s.UndeliveredReports(ctx, tcOwnerB)
	if err != nil || len(bs) != 1 {
		t.Fatalf("B's report must still be waiting: n=%d err=%v", len(bs), err)
	}
}
