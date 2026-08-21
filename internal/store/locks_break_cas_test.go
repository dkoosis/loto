package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"loto/internal/domain"
)

// readHolds is what a caller does before deciding to break: read the target's
// holders and keep their (owner, epoch) identities. Every test below opens
// with it, so the window the compare-and-swap closes is the window between
// this call and the BreakLocks call — no seam, no sleep, just two store calls
// with the interference written in between.
func readHolds(t *testing.T, s *Store, target domain.Target) []domain.HoldRef {
	t.Helper()
	rows, err := s.LocksAt(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]domain.HoldRef, len(rows))
	for i := range rows {
		refs[i] = domain.HoldRef{Owner: rows[i].OwnerUUID, Epoch: rows[i].Epoch}
	}
	domain.SortHoldRefs(refs)
	return refs
}

// ownersAt names who currently holds target, in the store's deterministic order.
func ownersAt(t *testing.T, s *Store, target domain.Target) []string {
	t.Helper()
	rows, err := s.LocksAt(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = string(rows[i].OwnerUUID)
	}
	return out
}

// asHolderChanged unwraps the CAS rejection, failing the test when the result
// is any other outcome. Both hold sets are part of the contract — a refusal
// that can't say what it found is the silence loto-tqcw exists to end.
func asHolderChanged(t *testing.T, err error) *HolderChangedError {
	t.Helper()
	if !errors.Is(err, ErrHolderChanged) {
		t.Fatalf("want ErrHolderChanged, got %v", err)
	}
	var hc *HolderChangedError
	if !errors.As(err, &hc) {
		t.Fatalf("ErrHolderChanged must carry a *HolderChangedError, got %v", err)
	}
	return hc
}

// mustBeHolderChanged is asHolderChanged for the cases that only need the
// verdict. Separate rather than discarding the return: *HolderChangedError is
// an error type, so `asHolderChanged(...)` as a bare statement reads to
// errcheck as an unchecked error.
func mustBeHolderChanged(t *testing.T, err error) {
	t.Helper()
	_ = asHolderChanged(t, err) // asHolderChanged already failed the test on mismatch
}

// TestBreakLocks_ThirdAgentReacquiredBetweenReadAndBreak is the race the bead
// describes, run end to end: bob reads alice's hold, alice releases, carol
// takes the path, bob's break lands. Carol must keep her lock and bob must be
// told why.
//
// ‡ Deterministic without a seam. The dangerous window is NOT inside
// BreakLocks — withLockBatchTx takes the op-flock before its read and holds it
// past commit, so nothing moves under the store. The window is between bob's
// own two calls, which this test drives in sequence. That is exactly why the
// bug was invisible: the store's own critical section was already airtight,
// and the stale decision arrived from outside it.
func TestBreakLocks_ThirdAgentReacquiredBetweenReadAndBreak(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alice := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, liveProbe); err != nil {
		t.Fatal(err)
	}

	// 1. bob reads: alice holds it.
	seen := readHolds(t, s, alice.Target)
	if len(seen) != 1 || seen[0].Owner != tcAlice {
		t.Fatalf("setup: want a single alice hold, got %v", seen)
	}

	// 2. between the read and the break, alice releases and carol takes it.
	if _, err := s.ReleaseLocks(ctx, []domain.Target{alice.Target}, tcAlice, liveProbe); err != nil {
		t.Fatal(err)
	}
	carol := alice
	carol.OwnerUUID, carol.SessionUUID = tcCarol, tcCarol
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{carol}, liveProbe); err != nil {
		t.Fatal(err)
	}

	// 3. bob breaks against the hold he read.
	res, err := s.BreakLocks(ctx, []domain.Target{alice.Target}, tcBob, BreakForce, "taking over",
		liveProbe, BreakExpectations{alice.Target.Canonical: seen})
	if err != nil {
		t.Fatal(err)
	}
	hc := asHolderChanged(t, res[0].Err)
	if len(hc.Expected) != 1 || hc.Expected[0] != seen[0] {
		t.Errorf("refusal must echo what bob stated, got %v", hc.Expected)
	}
	if len(hc.Actual) != 1 || hc.Actual[0].Owner != tcCarol {
		t.Errorf("refusal must name carol as the current holder, got %v", hc.Actual)
	}

	// 4. carol keeps her lock, and nothing was audited as broken — a refused
	//    break dispossesses nobody, so invariant 8 has nothing to record.
	if owners := ownersAt(t, s, alice.Target); len(owners) != 1 || owners[0] != tcCarol {
		t.Errorf("carol must still hold the target, holders now %v", owners)
	}
	if n := countEvents(t, s, alice.Target, EventLockBroken); n != 0 {
		t.Errorf("refused break must emit no lock_broken event, got %d", n)
	}
}

// TestBreakLocks_SameOwnerReacquireIsADifferentHold is the case an
// owner-UUID-only compare-and-swap would wave through: alice releases and
// takes the path again, so the owner never changed but the hold did. The
// epoch qualifier is the only reason this refuses.
func TestBreakLocks_SameOwnerReacquireIsADifferentHold(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alice := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, liveProbe); err != nil {
		t.Fatal(err)
	}
	seen := readHolds(t, s, alice.Target)

	if _, err := s.ReleaseLocks(ctx, []domain.Target{alice.Target}, tcAlice, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, liveProbe); err != nil {
		t.Fatal(err)
	}

	res, err := s.BreakLocks(ctx, []domain.Target{alice.Target}, tcBob, BreakForce, "stale read",
		liveProbe, BreakExpectations{alice.Target.Canonical: seen})
	if err != nil {
		t.Fatal(err)
	}
	hc := asHolderChanged(t, res[0].Err)
	if hc.Actual[0].Owner != tcAlice {
		t.Fatalf("owner should be unchanged, got %v", hc.Actual)
	}
	if hc.Actual[0].Epoch == hc.Expected[0].Epoch {
		t.Errorf("re-acquire must advance the epoch; expected %v == actual %v", hc.Expected, hc.Actual)
	}
	if owners := ownersAt(t, s, alice.Target); len(owners) != 1 {
		t.Errorf("alice's new hold must survive, holders now %v", owners)
	}
}

// TestBreakLocks_MatchingExpectationBreaks: the swap half of compare-and-swap.
// Nothing moved, so the break lands and audits exactly as a blind one would.
func TestBreakLocks_MatchingExpectationBreaks(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alice := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, liveProbe); err != nil {
		t.Fatal(err)
	}
	seen := readHolds(t, s, alice.Target)

	res, err := s.BreakLocks(ctx, []domain.Target{alice.Target}, tcBob, BreakForce, "deadline",
		liveProbe, BreakExpectations{alice.Target.Canonical: seen})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Err != nil {
		t.Fatalf("matching expectation must break, got %v", res[0].Err)
	}
	if owners := ownersAt(t, s, alice.Target); len(owners) != 0 {
		t.Errorf("target must be free after the break, holders %v", owners)
	}
	if n := countEvents(t, s, alice.Target, EventLockBroken); n != 1 {
		t.Errorf("want one lock_broken event (invariant 8), got %d", n)
	}
}

// TestBreakLocks_LeaseRefreshKeepsTheSameHold pins the reason the token is the
// path epoch and not created_at: a holder renewing its lease has not changed
// hold, so a break authorized against the pre-refresh read must still land.
// A created_at-keyed ref would refuse here, turning every heartbeat into a
// spurious rejection.
func TestBreakLocks_LeaseRefreshKeepsTheSameHold(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alice := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, liveProbe); err != nil {
		t.Fatal(err)
	}
	seen := readHolds(t, s, alice.Target)

	if _, err := s.RefreshLocks(ctx, []domain.Target{alice.Target}, tcAlice, 2*time.Hour); err != nil {
		t.Fatal(err)
	}

	res, err := s.BreakLocks(ctx, []domain.Target{alice.Target}, tcBob, BreakForce, "deadline",
		liveProbe, BreakExpectations{alice.Target.Canonical: seen})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Err != nil {
		t.Fatalf("a lease refresh is the same hold; break must still land, got %v", res[0].Err)
	}
}

// TestBreakLocks_SharedCoHolderJoinedSinceTheRead: shared mode makes "the
// holder" a set, and the compare is exact. A reader that joined after the
// caller looked is one the caller never authorized breaking, so its arrival
// must refuse the whole target rather than break some holders and leave others
// (the loto-w77f all-or-nothing rule, now applied to the caller's statement).
func TestBreakLocks_SharedCoHolderJoinedSinceTheRead(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alice := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	alice.Mode = domain.ModeShared
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, liveProbe); err != nil {
		t.Fatal(err)
	}
	seen := readHolds(t, s, alice.Target)

	carol := peerOn(alice, tcCarol, domain.ModeShared)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{carol}, liveProbe); err != nil {
		t.Fatal(err)
	}

	res, err := s.BreakLocks(ctx, []domain.Target{alice.Target}, tcBob, BreakForce, "clearing readers",
		liveProbe, BreakExpectations{alice.Target.Canonical: seen})
	if err != nil {
		t.Fatal(err)
	}
	hc := asHolderChanged(t, res[0].Err)
	if len(hc.Actual) != 2 {
		t.Errorf("refusal must report both live readers, got %v", hc.Actual)
	}
	if owners := ownersAt(t, s, alice.Target); len(owners) != 2 {
		t.Errorf("both readers must survive, holders now %v", owners)
	}
}

// TestBreakLocks_SharedFullSetStatedBreaksAll: state every holder and the
// break lands on all of them, one audit event each.
func TestBreakLocks_SharedFullSetStatedBreaksAll(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alice := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	alice.Mode = domain.ModeShared
	carol := peerOn(alice, tcCarol, domain.ModeShared)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{carol}, liveProbe); err != nil {
		t.Fatal(err)
	}
	seen := readHolds(t, s, alice.Target)
	if len(seen) != 2 {
		t.Fatalf("setup: want two shared holds, got %v", seen)
	}

	res, err := s.BreakLocks(ctx, []domain.Target{alice.Target}, tcBob, BreakForce, "clearing readers",
		liveProbe, BreakExpectations{alice.Target.Canonical: seen})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Err != nil {
		t.Fatalf("full holder set stated: break must land, got %v", res[0].Err)
	}
	if owners := ownersAt(t, s, alice.Target); len(owners) != 0 {
		t.Errorf("target must be free, holders %v", owners)
	}
	if n := countEvents(t, s, alice.Target, EventLockBroken); n != 2 {
		t.Errorf("want one lock_broken per dispossessed holder, got %d", n)
	}
}

// TestBreakLocks_ExpectedHoldVanished: the hold the caller read is gone and
// nothing replaced it. That is a failed compare, not a plain miss — reporting
// ErrNoLockAtTarget here would tell the caller "there was never anything to
// break" when in truth someone released underneath it.
func TestBreakLocks_ExpectedHoldVanished(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alice := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, liveProbe); err != nil {
		t.Fatal(err)
	}
	seen := readHolds(t, s, alice.Target)
	if _, err := s.ReleaseLocks(ctx, []domain.Target{alice.Target}, tcAlice, liveProbe); err != nil {
		t.Fatal(err)
	}

	res, err := s.BreakLocks(ctx, []domain.Target{alice.Target}, tcBob, BreakForce, "taking over",
		liveProbe, BreakExpectations{alice.Target.Canonical: seen})
	if err != nil {
		t.Fatal(err)
	}
	hc := asHolderChanged(t, res[0].Err)
	if errors.Is(res[0].Err, ErrNoLockAtTarget) {
		t.Errorf("a vanished expected hold is a failed compare, not a miss")
	}
	if len(hc.Actual) != 0 {
		t.Errorf("actual set must be empty, got %v", hc.Actual)
	}
	if got := hc.Error(); got == "" {
		t.Errorf("refusal must have a message")
	}
}

// TestBreakLocks_NoExpectationIsBlindBreak: a nil map is the pre-loto-tqcw
// behavior, unchanged. The blind form stays reachable for sweeps over dead
// territory, where there is no generation to name.
func TestBreakLocks_NoExpectationIsBlindBreak(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alice := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, liveProbe); err != nil {
		t.Fatal(err)
	}
	res, err := s.BreakLocks(ctx, []domain.Target{alice.Target}, tcBob, BreakForce, "sweep", liveProbe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Err != nil {
		t.Fatalf("blind break must still work, got %v", res[0].Err)
	}
	if owners := ownersAt(t, s, alice.Target); len(owners) != 0 {
		t.Errorf("target must be free, holders %v", owners)
	}
}

// TestBreakLocks_RefusalDoesNotAbortTheBatch: per-target outcomes stay
// per-target. One refused compare must not roll back or skip the siblings, and
// results must still come back in input order.
func TestBreakLocks_RefusalDoesNotAbortTheBatch(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	b := mkFileLock(t, tcBGo, tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a, b}, liveProbe); err != nil {
		t.Fatal(err)
	}
	stale := []domain.HoldRef{{Owner: tcAlice, Epoch: 99}} // a generation that never existed

	res, err := s.BreakLocks(ctx, []domain.Target{a.Target, b.Target}, tcBob, BreakForce, "mixed",
		liveProbe, BreakExpectations{a.Target.Canonical: stale})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Target.Canonical != a.Target.Canonical || res[1].Target.Canonical != b.Target.Canonical {
		t.Fatalf("results must stay in input order, got %v", res)
	}
	mustBeHolderChanged(t, res[0].Err)
	if res[1].Err != nil {
		t.Errorf("the un-expected target must still break, got %v", res[1].Err)
	}
	if owners := ownersAt(t, s, a.Target); len(owners) != 1 {
		t.Errorf("refused target keeps its holder, got %v", owners)
	}
	if owners := ownersAt(t, s, b.Target); len(owners) != 0 {
		t.Errorf("un-expected target must be free, got %v", owners)
	}
}

// TestBreakLocks_StaleModeHonorsExpectation: the compare is a property of the
// batch, not of BreakForce. A stale-only reclaim that names the wrong hold is
// refused on the same terms.
func TestBreakLocks_StaleModeHonorsExpectation(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alice := mkFileLock(t, tcAGo, tcAlice, -time.Minute) // already expired
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, deadProbe); err != nil {
		t.Fatal(err)
	}
	wrong := []domain.HoldRef{{Owner: tcCarol, Epoch: 1}}

	res, err := s.BreakLocks(ctx, []domain.Target{alice.Target}, tcBob, BreakStale, "reclaim",
		deadProbe, BreakExpectations{alice.Target.Canonical: wrong})
	if err != nil {
		t.Fatal(err)
	}
	mustBeHolderChanged(t, res[0].Err)
	if owners := ownersAt(t, s, alice.Target); len(owners) != 1 {
		t.Errorf("stale row must survive a failed compare, holders %v", owners)
	}
}
