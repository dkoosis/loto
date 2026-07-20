package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"loto/internal/domain"
)

func mkClaim(prefix, owner string, expIn time.Duration) domain.ClaimRecord {
	now := time.Now()
	return domain.ClaimRecord{
		PathPrefix:  prefix,
		OwnerUUID:   domain.AgentUUID(owner),
		SessionUUID: domain.SessionUUID(owner),
		Intent:      tcTest,
		CreatedAt:   now,
		ExpiresAt:   now.Add(expIn),
		Host:        "h",
	}
}

// mkClaimSession is mkClaim with an explicit session, for the session-scoped
// release paths (mkClaim defaults SessionUUID to the owner).
func mkClaimSession(prefix, owner, session string, expIn time.Duration) domain.ClaimRecord {
	c := mkClaim(prefix, owner, expIn)
	c.SessionUUID = domain.SessionUUID(session)
	return c
}

// ReleaseBySession with a session filter drops only that session's claims for
// the agent, leaving another session's claim and other agents' claims intact
// (loto-ei5). The released prefixes come back sorted, as the 2nd return.
func TestReleaseBySession_ClaimsScopedToSession(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	for _, c := range []domain.ClaimRecord{
		mkClaimSession("internal/z", tcAlice, "session-1", time.Hour),
		mkClaimSession("internal/a", tcAlice, "session-1", time.Hour),
		mkClaimSession("internal/b", tcAlice, "session-2", time.Hour),
		mkClaimSession("internal/c", tcBob, "session-1", time.Hour),
	} {
		if err := s.ClaimPrefix(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	_, got, err := s.ReleaseBySession(ctx, tcAlice, "session-1")
	if err != nil {
		t.Fatalf("ReleaseBySession: %v", err)
	}
	want := []string{"internal/a", "internal/z"} // sorted, session-1 only
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("released = %v, want %v (sorted, session-1 alice only)", got, want)
	}

	remaining, err := s.ListClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// alice/session-2 and bob/session-1 survive.
	surviving := map[string]bool{}
	for _, c := range remaining {
		surviving[c.PathPrefix] = true
	}
	if !surviving["internal/b"] || !surviving["internal/c"] || len(remaining) != 2 {
		t.Fatalf("survivors = %v, want internal/b + internal/c", remaining)
	}
}

// An empty session filter is the agent-scoped fallback: every one of the agent's
// claims goes, across sessions, but other agents' claims stay.
func TestReleaseBySession_ClaimsAgentScopedFallback(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	for _, c := range []domain.ClaimRecord{
		mkClaimSession("internal/a", tcAlice, "session-1", time.Hour),
		mkClaimSession("internal/b", tcAlice, "session-2", time.Hour),
		mkClaimSession("internal/c", tcBob, "session-1", time.Hour),
	} {
		if err := s.ClaimPrefix(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	_, got, err := s.ReleaseBySession(ctx, tcAlice, "")
	if err != nil {
		t.Fatalf("ReleaseBySession: %v", err)
	}
	if len(got) != 2 || got[0] != "internal/a" || got[1] != "internal/b" {
		t.Fatalf("released = %v, want both alice prefixes across sessions", got)
	}
	remaining, err := s.ListClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].OwnerUUID != domain.AgentUUID(tcBob) {
		t.Fatalf("only bob's claim should survive, got %v", remaining)
	}
}

// Nothing owned → nil, no error, and no write (empty result is not a failure).
func TestReleaseBySession_ClaimsNothingOwned(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.ClaimPrefix(ctx, mkClaimSession("internal/c", tcBob, "session-1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, got, err := s.ReleaseBySession(ctx, tcAlice, "")
	if err != nil {
		t.Fatalf("ReleaseBySession: %v", err)
	}
	if got != nil {
		t.Fatalf("released = %v, want nil for an agent owning no claims", got)
	}
}

func TestClaimPrefixBlocksOverlap(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.ClaimPrefix(ctx, mkClaim("internal/store", tcAlice, time.Hour)); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, prefix string }{
		{"exact", "internal/store"},
		{"child", "internal/store/sub"},
		{"parent", "internal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.ClaimPrefix(ctx, mkClaim(c.prefix, tcBob, time.Hour))
			var cce *ClaimConflictError
			if !errors.As(err, &cce) {
				t.Fatalf("ClaimPrefix(%q) err=%v; want *ClaimConflictError", c.prefix, err)
			}
			if len(cce.Blockers) != 1 {
				t.Fatalf("blockers=%d; want 1: %+v", len(cce.Blockers), cce.Blockers)
			}
			if cce.Blockers[0].OwnerUUID != domain.AgentUUID(tcAlice) {
				t.Errorf("blocker owner=%s; want %s", cce.Blockers[0].OwnerUUID, tcAlice)
			}
		})
	}
}

func TestClaimPrefixSameOwnerRefresh(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	first := mkClaim("internal/store", tcAlice, time.Minute)
	if err := s.ClaimPrefix(ctx, first); err != nil {
		t.Fatal(err)
	}
	refresh := mkClaim("internal/store", tcAlice, time.Hour)
	refresh.Intent = "claim-refreshed"
	if err := s.ClaimPrefix(ctx, refresh); err != nil {
		t.Fatalf("same-owner re-claim must refresh, got: %v", err)
	}
	all, err := s.ListClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("rows=%d; want 1 (upsert): %+v", len(all), all)
	}
	got := all[0]
	if got.Intent != "claim-refreshed" {
		t.Errorf("intent=%q; want refreshed", got.Intent)
	}
	if !got.ExpiresAt.After(first.ExpiresAt) {
		t.Errorf("expires_at not extended: %v !> %v", got.ExpiresAt, first.ExpiresAt)
	}
	// created_at is preserved across refresh (mirrors insertOrRefreshLock).
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at changed on refresh: %v vs %v", got.CreatedAt, first.CreatedAt)
	}
	// Same-owner overlap on a DIFFERENT prefix never blocks either.
	if err := s.ClaimPrefix(ctx, mkClaim("internal/store/sub", tcAlice, time.Hour)); err != nil {
		t.Fatalf("same-owner nested claim must not block: %v", err)
	}
}

func TestClaimPrefixSiblingsCoexist(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.ClaimPrefix(ctx, mkClaim("internal/store", tcAlice, time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimPrefix(ctx, mkClaim("internal/storefront", tcBob, time.Hour)); err != nil {
		t.Fatalf("string-prefix sibling must not overlap: %v", err)
	}
	if err := s.ClaimPrefix(ctx, mkClaim("internal/render", tcBob, time.Hour)); err != nil {
		t.Fatalf("disjoint sibling must not overlap: %v", err)
	}
	all, err := s.ListClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("rows=%d; want 3: %+v", len(all), all)
	}
}

func TestClaimPrefixReclaimsExpired(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.ClaimPrefix(ctx, mkClaim("pkg/a", tcAlice, -time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimPrefix(ctx, mkClaim("pkg/a/deep", tcBob, time.Hour)); err != nil {
		t.Fatalf("expired overlapping claim must be reclaimed, got: %v", err)
	}
	all, err := s.ListClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("rows=%d; want 1 (expired row deleted in winner's txn): %+v", len(all), all)
	}
	if all[0].OwnerUUID != domain.AgentUUID(tcBob) || all[0].PathPrefix != "pkg/a/deep" {
		t.Errorf("surviving row = %+v; want bob's pkg/a/deep", all[0])
	}
}

func TestReleaseClaimOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.ClaimPrefix(ctx, mkClaim("internal/store", tcAlice, time.Hour)); err != nil {
		t.Fatal(err)
	}
	res, err := s.ReleaseClaim(ctx, "internal/store", tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != ClaimStateReleased {
		t.Fatalf("state=%v; want ClaimStateReleased: %+v", res.State, res)
	}
	all, err := s.ListClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("claim should be gone, got %+v", all)
	}
}

func TestReleaseClaimNoClaim(t *testing.T) {
	s := mustOpen(t)
	res, err := s.ReleaseClaim(context.Background(), "internal/store", tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != ClaimStateNoClaim {
		t.Fatalf("state=%v; want ClaimStateNoClaim: %+v", res.State, res)
	}
}

func TestReleaseClaimNotOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.ClaimPrefix(ctx, mkClaim("internal/store", tcAlice, time.Hour)); err != nil {
		t.Fatal(err)
	}
	res, err := s.ReleaseClaim(ctx, "internal/store", tcBob)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != ClaimStateNotOwner {
		t.Fatalf("state=%v; want ClaimStateNotOwner: %+v", res.State, res)
	}
	if res.Owner != tcAlice {
		t.Errorf("owner=%q; want %q (row names actual owner)", res.Owner, tcAlice)
	}
	// Alice's claim must be untouched.
	all, err := s.ListClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].OwnerUUID != domain.AgentUUID(tcAlice) {
		t.Fatalf("alice's claim must survive bob's unclaim: %+v", all)
	}
}

// TestReleaseClaimIgnoresExpiredForeignRow pins the pass-2 persistence fix:
// a prefix held only by an EXPIRED foreign claim reports no-claim (exit 0),
// not not-owner — the probe must apply the same staleness semantics as
// partitionClaims and status, or a dead lease blocks an unclaim verdict it
// has no live right to.
func TestReleaseClaimIgnoresExpiredForeignRow(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.ClaimPrefix(ctx, mkClaim("internal/store", tcAlice, -time.Second)); err != nil {
		t.Fatal(err)
	}
	res, err := s.ReleaseClaim(ctx, "internal/store", tcBob)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != ClaimStateNoClaim {
		t.Fatalf("state=%v; want ClaimStateNoClaim (expired row must not report not-owner): %+v", res.State, res)
	}
}

// TestClaimsTableEnsuredOnExistingDB pins the no-version-bump migration: a DB
// stamped at the current user_version but predating the claims table gets the
// table via the ensureClaimsTable step in migrationEnsures on the next Open.
func TestClaimsTableEnsuredOnExistingDB(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/loto.db"
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-claims DB: drop the table, keep user_version stamped.
	if _, err := s.db.ExecContext(context.Background(), `DROP TABLE claims; DROP INDEX IF EXISTS idx_claims_expires`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(p)
	if err != nil {
		t.Fatalf("re-open on pre-claims DB: %v", err)
	}
	defer s2.Close()
	if err := s2.ClaimPrefix(context.Background(), mkClaim("internal/store", tcAlice, time.Hour)); err != nil {
		t.Fatalf("claims table not ensured on existing DB: %v", err)
	}
}
