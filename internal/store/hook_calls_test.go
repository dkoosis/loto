package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"loto/internal/domain"
)

const (
	tcHookPath  = "internal/hooked.go"
	tcHookPath2 = "internal/other_hooked.go"
	tcOwnerA    = "owner-a"
	tcOwnerB    = "owner-b"
)

func hookObs(path, digest string) HookPathState {
	return HookPathState{Path: path, Locked: true, Epoch: 1, Holder: tcOwnerA, Digest: digest, Stat: "10:420:1"}
}

func mustPre(t *testing.T, s *Store, callID string, tPre time.Time, obs ...HookPathState) {
	t.Helper()
	ok, err := s.RecordCallPre(context.Background(), HookCall{
		CallID: callID, OwnerUUID: tcOwnerA, SessionUUID: "sess-a", ToolName: "Bash", TPre: tPre,
	}, obs)
	if err != nil {
		t.Fatalf("pre %s: %v", callID, err)
	}
	if !ok {
		t.Fatalf("pre %s: want recorded, got no-op", callID)
	}
}

func TestMigrate_AddsHookCallTables(t *testing.T) {
	s := mustOpen(t)
	for _, table := range []string{"hook_calls", "hook_call_paths", "path_seq"} {
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

// A pre then a post over one locked file leaves ONE call record carrying
// seq_pre, seq_post and both digests — the bead's first acceptance criterion,
// at the store layer.
func TestRecordCall_PreThenPostHoldsBothDigests(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	mustPre(t, s, "call-1", now, hookObs(tcHookPath, tcSHA1))
	out, err := s.RecordCallPost(ctx, "call-1", now.Add(time.Second), []HookPathState{
		{Path: tcHookPath, Locked: true, Epoch: 1, Holder: tcOwnerA, Digest: tcSHA2, Stat: "11:420:2"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if !out.Recorded || len(out.Changed) != 1 || out.Changed[0] != tcHookPath {
		t.Fatalf("want one changed path recorded, got %+v", out)
	}

	call, paths, ok, err := s.CallRecord(ctx, "call-1")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if call.InFlight() {
		t.Error("posted call still reads as in flight")
	}
	if len(paths) != 1 {
		t.Fatalf("want 1 path row, got %d", len(paths))
	}
	p := paths[0]
	if p.DigestPre != tcSHA1 || p.DigestPost != tcSHA2 {
		t.Errorf("digests: pre=%q post=%q", p.DigestPre, p.DigestPost)
	}
	if p.SeqPre != 0 || p.SeqPost != 1 {
		t.Errorf("changed path wants seq 0 -> 1, got %d -> %d", p.SeqPre, p.SeqPost)
	}
}

// A path whose digest and stat are identical across the call keeps its number;
// only a change takes the next one. Ordering is by seq, never by wall clock.
func TestRecordCallPost_UnchangedPathKeepsItsSeq(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	mustPre(t, s, "call-noop", now, hookObs(tcHookPath, tcSHA1))
	out, err := s.RecordCallPost(ctx, "call-noop", now.Add(time.Second), []HookPathState{
		hookObs(tcHookPath, tcSHA1),
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if len(out.Changed) != 0 {
		t.Fatalf("want no change, got %v", out.Changed)
	}
	_, paths, _, err := s.CallRecord(ctx, "call-noop")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if paths[0].SeqPre != paths[0].SeqPost {
		t.Errorf("unchanged path moved: %d -> %d", paths[0].SeqPre, paths[0].SeqPost)
	}
	seq, err := s.PathSeq(ctx, tcHookPath, 1)
	if err != nil {
		t.Fatalf("path seq: %v", err)
	}
	if seq != 0 {
		t.Errorf("want the transition sequence untouched at 0, got %d", seq)
	}
}

// A repeat post for the same tool_use_id changes nothing — not the timestamps,
// not the digests, and above all not the transition sequence.
func TestRecordCallPost_RepeatIsANoOp(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	mustPre(t, s, "call-dup", now, hookObs(tcHookPath, tcSHA1))
	if _, err := s.RecordCallPost(ctx, "call-dup", now.Add(time.Second), []HookPathState{
		{Path: tcHookPath, Locked: true, Epoch: 1, Holder: tcOwnerA, Digest: tcSHA2, Stat: "11:420:2"},
	}); err != nil {
		t.Fatalf("first post: %v", err)
	}
	before, pathsBefore, _, err := s.CallRecord(ctx, "call-dup")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	out, err := s.RecordCallPost(ctx, "call-dup", now.Add(time.Minute), []HookPathState{
		{Path: tcHookPath, Locked: true, Epoch: 1, Holder: tcOwnerA, Digest: tcBaseline, Stat: "99:420:9"},
	})
	if err != nil {
		t.Fatalf("second post: %v", err)
	}
	if out.Recorded {
		t.Fatal("second post reported a write")
	}
	after, pathsAfter, _, err := s.CallRecord(ctx, "call-dup")
	if err != nil {
		t.Fatalf("read back 2: %v", err)
	}
	if !after.TPost.Equal(before.TPost) {
		t.Errorf("t_post moved: %v -> %v", before.TPost, after.TPost)
	}
	if pathsAfter[0] != pathsBefore[0] {
		t.Errorf("path row moved:\n before %+v\n after  %+v", pathsBefore[0], pathsAfter[0])
	}
	seq, err := s.PathSeq(ctx, tcHookPath, 1)
	if err != nil {
		t.Fatalf("path seq: %v", err)
	}
	if seq != 1 {
		t.Errorf("want seq still 1 after the repeat, got %d", seq)
	}
}

// A post for a call_id nothing recorded a pre for is reported, not silently
// invented: a call whose opening nobody observed has no before-state to diff.
func TestRecordCallPost_UnknownCall(t *testing.T) {
	s := mustOpen(t)
	_, err := s.RecordCallPost(context.Background(), "never-seen", time.Now(), nil)
	if !errors.Is(err, ErrUnknownCall) {
		t.Fatalf("want ErrUnknownCall, got %v", err)
	}
}

// A repeated pre keeps the FIRST observation: that is the one taken before the
// tool ran, and a later re-read would record post-write bytes as the baseline.
func TestRecordCallPre_RepeatKeepsTheFirstObservation(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	mustPre(t, s, "call-rp", now, hookObs(tcHookPath, tcSHA1))
	ok, err := s.RecordCallPre(ctx, HookCall{CallID: "call-rp", OwnerUUID: tcOwnerA, TPre: now.Add(time.Minute)},
		[]HookPathState{hookObs(tcHookPath, tcSHA2)})
	if err != nil {
		t.Fatalf("repeat pre: %v", err)
	}
	if ok {
		t.Fatal("repeat pre reported a write")
	}
	_, paths, _, err := s.CallRecord(ctx, "call-rp")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(paths) != 1 || paths[0].DigestPre != tcSHA1 {
		t.Fatalf("want the first pre digest kept, got %+v", paths)
	}
}

// §10a test 9. A's post for call 41 never lands; A issues 42 and 43 inside
// T_report. 41 stays in flight; past T_report it is post_missing AND STILL in
// flight. 42 and 43 are unaffected, and neither one's pre closes 41.
func TestPostMissing_StaysInFlightAndLaterCallsDoNotCloseIt(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	const tReport = 10 * time.Minute
	t0 := time.Now()

	mustPre(t, s, "call-41", t0, hookObs(tcHookPath, tcSHA1))
	mustPre(t, s, "call-42", t0.Add(time.Second), hookObs(tcHookPath, tcSHA1))
	if _, err := s.RecordCallPost(ctx, "call-42", t0.Add(2*time.Second), []HookPathState{hookObs(tcHookPath, tcSHA1)}); err != nil {
		t.Fatalf("post 42: %v", err)
	}
	mustPre(t, s, "call-43", t0.Add(3*time.Second), hookObs(tcHookPath, tcSHA1))
	if _, err := s.RecordCallPost(ctx, "call-43", t0.Add(4*time.Second), []HookPathState{hookObs(tcHookPath, tcSHA1)}); err != nil {
		t.Fatalf("post 43: %v", err)
	}

	// Inside T_report: 41 in flight, not yet flagged.
	n, err := s.MarkPostMissing(ctx, t0.Add(time.Minute), tReport)
	if err != nil {
		t.Fatalf("sweep inside T_report: %v", err)
	}
	if n != 0 {
		t.Errorf("swept %d calls inside T_report, want 0", n)
	}
	inflight, err := s.InFlightCalls(ctx)
	if err != nil {
		t.Fatalf("in flight: %v", err)
	}
	if len(inflight) != 1 || inflight[0].CallID != "call-41" || inflight[0].PostMissing {
		t.Fatalf("want only 41 in flight and unflagged, got %+v", inflight)
	}

	// Past T_report: flagged, still in flight.
	if n, err = s.MarkPostMissing(ctx, t0.Add(tReport+time.Minute), tReport); err != nil {
		t.Fatalf("sweep past T_report: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d calls past T_report, want 1", n)
	}
	inflight, err = s.InFlightCalls(ctx)
	if err != nil {
		t.Fatalf("in flight 2: %v", err)
	}
	if len(inflight) != 1 || inflight[0].CallID != "call-41" {
		t.Fatalf("41 left the in-flight set: %+v", inflight)
	}
	if !inflight[0].PostMissing || !inflight[0].InFlight() {
		t.Errorf("want post_missing AND in flight, got missing=%v inflight=%v",
			inflight[0].PostMissing, inflight[0].InFlight())
	}
}

// Retention: a finished call is kept while an older-opening call is still in
// flight, and dropped once none is — with the floor holding a just-posted
// record long enough to be read.
func TestDropFinishedCalls_KeptWhileAnOlderCallIsInFlight(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)

	mustPre(t, s, "call-open", t0, hookObs(tcHookPath, tcSHA1))
	mustPre(t, s, "call-done", t0.Add(time.Second), hookObs(tcHookPath2, tcSHA1))
	if _, err := s.RecordCallPost(ctx, "call-done", t0.Add(2*time.Second), []HookPathState{hookObs(tcHookPath2, tcSHA1)}); err != nil {
		t.Fatalf("post: %v", err)
	}

	n, err := s.DropFinishedCalls(ctx, time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("drop with an open older call: %v", err)
	}
	if n != 0 {
		t.Fatalf("dropped %d while call-open is in flight, want 0", n)
	}

	if _, err := s.RecordCallPost(ctx, "call-open", t0.Add(3*time.Second), []HookPathState{hookObs(tcHookPath, tcSHA1)}); err != nil {
		t.Fatalf("post open: %v", err)
	}
	if n, err = s.DropFinishedCalls(ctx, time.Now(), time.Minute); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if n != 2 {
		t.Fatalf("dropped %d with nothing in flight, want 2", n)
	}
	var paths int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM hook_call_paths`).Scan(&paths); err != nil {
		t.Fatalf("count paths: %v", err)
	}
	if paths != 0 {
		t.Errorf("path rows survived their calls: %d", paths)
	}
}

// The floor keeps a just-posted record readable: a post landing with nothing
// else in flight must not delete its own evidence in the same instant.
func TestDropFinishedCalls_FloorKeepsAFreshPost(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	mustPre(t, s, "call-fresh", now, hookObs(tcHookPath, tcSHA1))
	if _, err := s.RecordCallPost(ctx, "call-fresh", now, []HookPathState{hookObs(tcHookPath, tcSHA1)}); err != nil {
		t.Fatalf("post: %v", err)
	}
	n, err := s.DropFinishedCalls(ctx, now, 10*time.Minute)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if n != 0 {
		t.Fatalf("dropped %d fresh records, want 0", n)
	}
}

// A path that becomes dirty inside the call was never recorded at pre; it is
// still an observation, and it takes the next number (§5 I4 step 5).
func TestRecordCallPost_PathFirstSeenAtPostTakesTheNextSeq(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	mustPre(t, s, "call-new", now, hookObs(tcHookPath, tcSHA1))
	out, err := s.RecordCallPost(ctx, "call-new", now.Add(time.Second), []HookPathState{
		hookObs(tcHookPath, tcSHA1),
		{Path: tcHookPath2, Digest: tcSHA2, Stat: "3:420:4"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if len(out.Changed) != 1 || out.Changed[0] != tcHookPath2 {
		t.Fatalf("want only the newly dirty path changed, got %v", out.Changed)
	}
	_, paths, _, err := s.CallRecord(ctx, "call-new")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var found bool
	for _, p := range paths {
		if p.Path != tcHookPath2 {
			continue
		}
		found = true
		if p.PreObserved {
			t.Error("a path first seen at post is marked pre-observed")
		}
		if p.SeqPre != 0 || p.SeqPost != 1 {
			t.Errorf("want seq 0 -> 1, got %d -> %d", p.SeqPre, p.SeqPost)
		}
		if p.DigestPre != "" {
			t.Errorf("want an empty pre digest, got %q", p.DigestPre)
		}
	}
	if !found {
		t.Fatalf("the newly dirty path was not recorded: %+v", paths)
	}
}

// seq is keyed (f, E): the same path under a new lock epoch counts from zero,
// so a number can never be compared across a change of holder.
func TestPathSeq_IsKeyedToTheEpoch(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	mustPre(t, s, "call-e1", now, hookObs(tcHookPath, tcSHA1))
	if _, err := s.RecordCallPost(ctx, "call-e1", now.Add(time.Second), []HookPathState{
		{Path: tcHookPath, Locked: true, Epoch: 1, Holder: tcOwnerA, Digest: tcSHA2, Stat: "2:420:2"},
	}); err != nil {
		t.Fatalf("post e1: %v", err)
	}
	seq1, err := s.PathSeq(ctx, tcHookPath, 1)
	if err != nil {
		t.Fatalf("seq epoch 1: %v", err)
	}
	seq2, err := s.PathSeq(ctx, tcHookPath, 2)
	if err != nil {
		t.Fatalf("seq epoch 2: %v", err)
	}
	if seq1 != 1 || seq2 != 0 {
		t.Errorf("want epoch 1 at seq 1 and epoch 2 at 0, got %d and %d", seq1, seq2)
	}
}

// A call whose owner the probe finds dead is ENDED — §3's second ending. It
// leaves the in-flight set and stops pinning finished records against
// retention, which is what made these two tables grow without bound after any
// session crash.
func TestMarkDeadOwnerCalls_EndsTheCallAndUnblocksRetention(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)

	// A crashed session's call, opened first, never posted.
	if _, err := s.RecordCallPre(ctx, HookCall{
		CallID: "call-crashed", OwnerUUID: tcOwnerB, SessionUUID: "sess-dead", TPre: t0,
	}, []HookPathState{hookObs(tcHookPath, tcSHA1)}); err != nil {
		t.Fatalf("pre crashed: %v", err)
	}
	// A live session's call that opened later and posted.
	mustPre(t, s, "call-live", t0.Add(time.Second), hookObs(tcHookPath2, tcSHA1))
	if _, err := s.RecordCallPost(ctx, "call-live", t0.Add(2*time.Second),
		[]HookPathState{hookObs(tcHookPath2, tcSHA1)}); err != nil {
		t.Fatalf("post live: %v", err)
	}

	// Before the sweep the crashed call pins the finished one forever.
	n, err := s.DropFinishedCalls(ctx, time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("drop before sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("dropped %d while a crashed call is in flight, want 0", n)
	}

	// A not-dead verdict must end nothing: only a provable death does.
	if n, err = s.MarkDeadOwnerCalls(ctx, time.Now(), func(domain.SessionUUID) bool { return false }); err != nil {
		t.Fatalf("sweep with a live verdict: %v", err)
	}
	if n != 0 {
		t.Fatalf("ended %d calls on a not-dead verdict, want 0", n)
	}

	dead := func(sess domain.SessionUUID) bool { return sess == "sess-dead" }
	if n, err = s.MarkDeadOwnerCalls(ctx, time.Now(), dead); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("ended %d calls, want 1", n)
	}

	inflight, err := s.InFlightCalls(ctx)
	if err != nil {
		t.Fatalf("in flight: %v", err)
	}
	if len(inflight) != 0 {
		t.Fatalf("a dead owner's call is still in flight: %+v", inflight)
	}
	call, _, ok, err := s.CallRecord(ctx, "call-crashed")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if call.InFlight() || call.DeadAt.IsZero() {
		t.Errorf("want ended with a dead_at, got inflight=%v dead_at=%v", call.InFlight(), call.DeadAt)
	}
	if !call.TPost.IsZero() {
		t.Error("a dead-owner ending must not fake a post")
	}

	// Retention reaches the finished call it was holding down. The crashed
	// call's own record is inside the floor — it ended a moment ago — so it
	// survives this pass, exactly as a fresh post does.
	if n, err = s.DropFinishedCalls(ctx, time.Now(), time.Minute); err != nil {
		t.Fatalf("drop after sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("dropped %d after the dead owner was swept, want the 1 it was pinning", n)
	}
	// Past the floor, the crashed call's own record goes too — the growth this
	// whole sweep exists to stop.
	if n, err = s.DropFinishedCalls(ctx, time.Now().Add(2*time.Minute), time.Minute); err != nil {
		t.Fatalf("drop past the floor: %v", err)
	}
	if n != 1 {
		t.Errorf("dropped %d past the floor, want the crashed call's own record", n)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM hook_calls`).Scan(&left); err != nil {
		t.Fatalf("count calls: %v", err)
	}
	if left != 0 {
		t.Errorf("%d call records survived a crashed session plus retention", left)
	}
}

// A call with no session id can never be probed, so it is never ended here —
// its owner may well be alive and about to post.
func TestMarkDeadOwnerCalls_LeavesAnUnjudgeableCallAlone(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if _, err := s.RecordCallPre(ctx, HookCall{
		CallID: "call-nosession", OwnerUUID: tcOwnerA, TPre: time.Now(),
	}, nil); err != nil {
		t.Fatalf("pre: %v", err)
	}
	n, err := s.MarkDeadOwnerCalls(ctx, time.Now(), func(domain.SessionUUID) bool { return true })
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("ended %d unjudgeable calls, want 0", n)
	}
}

// A peer's transition landing between the observation and the record must not
// exonerate this call from it. seq_pre is pinned to the number that held when
// the digest beside it was read, so the spanning test still sees the span.
func TestRecordCallPre_SeqPreIsPinnedToTheObservation(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	// The hook observes: seq is 0 here, and the bytes it reads are seq 0's.
	observed, err := s.PathSeq(ctx, tcHookPath, 1)
	if err != nil {
		t.Fatalf("observe seq: %v", err)
	}

	// A peer posts in the gap, assigning transition 1.
	mustPre(t, s, "call-peer", time.Now(), hookObs(tcHookPath, tcSHA1))
	if _, err := s.RecordCallPost(ctx, "call-peer", time.Now(), []HookPathState{
		{Path: tcHookPath, Locked: true, Epoch: 1, Holder: tcOwnerA, Digest: tcSHA2, Stat: "9:420:9"},
	}); err != nil {
		t.Fatalf("peer post: %v", err)
	}
	if n, serr := s.PathSeq(ctx, tcHookPath, 1); serr != nil || n != 1 {
		t.Fatalf("want the peer's transition at seq 1, got %d err=%v", n, serr)
	}

	// Only now does our record transaction run.
	if _, err := s.RecordCallPre(ctx, HookCall{
		CallID: "call-slow", OwnerUUID: tcOwnerB, SessionUUID: "sess-b", TPre: time.Now(),
	}, []HookPathState{{
		Path: tcHookPath, Locked: true, Epoch: 1, Holder: tcOwnerA,
		Digest: tcSHA1, Stat: "10:420:1",
		SeqAtObserve: observed, SeqAtObserveKnown: true,
	}}); err != nil {
		t.Fatalf("slow pre: %v", err)
	}
	_, paths, _, err := s.CallRecord(ctx, "call-slow")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if paths[0].SeqPre != 0 {
		t.Fatalf("seq_pre = %d: the in-tx read won over the observation, so the call is wrongly exonerated from transition 1", paths[0].SeqPre)
	}
}

// The contrast, pinned so a refactor that drops the observation's reading
// fails loudly: without it, seq_pre is whatever the transaction happened to see.
func TestRecordCallPre_WithoutTheObservationSeqPreFollowsTheTx(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	mustPre(t, s, "call-p", time.Now(), hookObs(tcHookPath, tcSHA1))
	if _, err := s.RecordCallPost(ctx, "call-p", time.Now(), []HookPathState{
		{Path: tcHookPath, Locked: true, Epoch: 1, Holder: tcOwnerA, Digest: tcSHA2, Stat: "9:420:9"},
	}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := s.RecordCallPre(ctx, HookCall{
		CallID: "call-unpinned", OwnerUUID: tcOwnerB, TPre: time.Now(),
	}, []HookPathState{hookObs(tcHookPath, tcSHA2)}); err != nil {
		t.Fatalf("pre: %v", err)
	}
	_, paths, _, err := s.CallRecord(ctx, "call-unpinned")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if paths[0].SeqPre != 1 {
		t.Errorf("want the in-tx value 1 with no observation reading, got %d", paths[0].SeqPre)
	}
}

// hook_timing cannot evict the rest of the audit trail. Its own cap holds it
// to its share; every other kind keeps the room the global cap gives it.
func TestRotateEvents_HookTimingHasItsOwnCap(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	// One low-rate row of another kind, written first so it is the OLDEST and
	// therefore the first casualty of a global-cap-only rotation.
	if _, err := s.AppendEvent(ctx, domain.Event{
		Kind: EventGateBypass, ActorUUID: tcOwnerA, Reason: "seed", CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	for i := range HookTimingRetentionMax + 50 {
		if _, err := s.AppendEventRotating(ctx, domain.Event{
			Kind: EventHookTiming, ActorUUID: tcOwnerA, Reason: "pre",
			CreatedAt: now.Add(time.Duration(i+1) * time.Millisecond),
		}); err != nil {
			t.Fatalf("timing row %d: %v", i, err)
		}
	}

	var timing, bypass int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE event_kind = ?`, EventHookTiming).Scan(&timing); err != nil {
		t.Fatalf("count timing: %v", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE event_kind = ?`, EventGateBypass).Scan(&bypass); err != nil {
		t.Fatalf("count bypass: %v", err)
	}
	if timing != HookTimingRetentionMax {
		t.Errorf("hook_timing rows = %d, want its cap of %d", timing, HookTimingRetentionMax)
	}
	if bypass != 1 {
		t.Errorf("the hook evicted an unrelated kind: gate_bypass rows = %d, want 1", bypass)
	}
}
