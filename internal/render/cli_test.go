package render

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

const (
	aGo        = "a.go"
	pkgA       = "pkg/a"
	bGo        = "b.go"
	cGo        = "c.go"
	ownerGreen = "Green"
	ownerRed   = "Red"
)

func TestEmitLockSuccess_SortedDeterministic(t *testing.T) {
	var buf bytes.Buffer
	EmitLockSuccess(&buf, []domain.LockRecord{
		{Target: domain.Target{Canonical: "z.go"}},
		{Target: domain.Target{Canonical: aGo}},
	})
	got := buf.String()
	wantHead := "✓ locked count=2\n"
	if !strings.HasPrefix(got, wantHead) {
		t.Errorf("first line want %q, got: %s", wantHead, got)
	}
	if strings.Index(got, "target=a.go") > strings.Index(got, "target=z.go") {
		t.Errorf("not sorted: %s", got)
	}
}

func TestEmitLockSuccess_ShowsMode(t *testing.T) {
	var buf bytes.Buffer
	EmitLockSuccess(&buf, []domain.LockRecord{
		{Target: domain.Target{Canonical: aGo}, Mode: domain.ModeShared},
	})
	if !strings.Contains(buf.String(), "mode=shared") {
		t.Fatalf("want mode=shared in: %q", buf.String())
	}
}

func TestEmitConflict_TriageFirst(t *testing.T) {
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	EmitConflictWithTags(&buf, &store.MultiConflictError{
		Blockers: []domain.LockRecord{
			{Target: domain.Target{Canonical: aGo}, OwnerUUID: ownerGreen, Intent: "x", ExpiresAt: now},
			{Target: domain.Target{Canonical: cGo}, OwnerUUID: ownerRed, Intent: "y", ExpiresAt: now},
		},
	}, nil)
	got := buf.String()
	if !strings.HasPrefix(got, "✗ blocked count=2\n") {
		t.Errorf("triage first: %s", got)
	}
}

func TestHolderTag_IsTheOwnerID(t *testing.T) {
	// The owner id is the Claude Code session id — the address SendMessage
	// already uses — so a report row prints it verbatim (loto-jnid).
	uuid := "00000000-0000-0000-0000-000000000000"
	if got := holderTag(uuid); got != uuid {
		t.Errorf("holderTag(%q) = %q, want the id itself", uuid, got)
	}
}

func TestEmitReleaseResults_MixedOutcomes(t *testing.T) {
	var buf bytes.Buffer
	exit := EmitReleaseResults(&buf, []store.ReleaseResult{
		{Target: domain.Target{Canonical: aGo}, State: store.StateUnlocked},
		{Target: domain.Target{Canonical: bGo}, State: store.StateNoLock},
		{Target: domain.Target{Canonical: cGo}, State: store.StateNotOwner, Owner: "BlueOak"},
	})
	if exit != 1 {
		t.Errorf("any not-owner → exit 1, got %d", exit)
	}
	got := buf.String()
	if !strings.Contains(got, "✓ unlocked count=1\n") {
		t.Errorf("triage count = successful releases only: %s", got)
	}
	if !strings.Contains(got, "state=no-lock") || !strings.Contains(got, "state=not-owner") {
		t.Errorf("missing distinct states: %s", got)
	}
	if !strings.Contains(got, "owner=BlueOak") {
		t.Errorf("not-owner row must name the lock owner with owner= field: %s", got)
	}
	if strings.Contains(got, "holder=") {
		t.Errorf("holder= field renamed to owner=, must not appear: %s", got)
	}
}

// TestEmitReleaseResults_SurfacesAuditHole covers loto-vmym's sibling loto-c6rg:
// a restore-failed release whose mode_restore_failed audit event was also lost
// must surface the audit hole to the operator (gh#107), not just the restore
// error. Pre-fix, ReleaseResult.AuditErr was populated but never rendered.
// TestEmitReleaseResults_RestoreFailedCountsAsUnlocked covers loto-qv91: a
// restore-failed release deleted the lock row in-tx (a successful unlock) but
// the chmod restore failed. The first-line triage count must count it as
// unlocked, not drop it to 0, while still surfacing the restore failures via a
// distinct restore-failed field. Pre-fix the header read "✓ unlocked count=0"
// for an all-restore-failed slice — actively misleading to the Claude consumer.
// TestEmitBreakResults_SurfacesRestoreAndAuditHoles covers loto-c6rg on the
// break path: unlock --force results carry RestoreErr/AuditErr that the prior
// inline renderer dropped — a forced break that left a file read-only or lost
// its audit event was silently reported as a clean "✓ broken".
func TestRelToCwd_AbsolutePathBecomesRelative(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(cwd, "sub", "x.go")
	got := relToCwd(abs, cwd)
	if got != filepath.Join("sub", "x.go") {
		t.Errorf("absolute should become cwd-relative, got %q", got)
	}
	// Already-relative input stays put.
	if relToCwd("sub/y.go", cwd) != "sub/y.go" {
		t.Errorf("relative input should pass through unchanged")
	}
	// Path that escapes cwd stays absolute.
	outside := filepath.Join(filepath.Dir(cwd), "elsewhere.go")
	if relToCwd(outside, cwd) != outside {
		t.Errorf("escaping path should stay absolute, got %q", relToCwd(outside, cwd))
	}
}

func TestEmitInvalid_DoesNotMutateInput(t *testing.T) {
	in := []InvalidTarget{
		{Path: "z.go", Reason: "not-found"},
		{Path: aGo, Reason: "symlink"},
	}
	original := []InvalidTarget{in[0], in[1]}
	var buf bytes.Buffer
	EmitInvalid(&buf, in)
	if in[0] != original[0] || in[1] != original[1] {
		t.Errorf("EmitInvalid must not mutate caller's slice; got %+v", in)
	}
	if !strings.HasPrefix(buf.String(), "✗ invalid count=2\n") {
		t.Errorf("triage first: %s", buf.String())
	}
}

func TestEmitTagFooter_Empty_NoOutput(t *testing.T) {
	var buf bytes.Buffer
	EmitTagFooter(&buf, nil, "alice")
	if buf.Len() != 0 {
		t.Fatalf("empty input must emit nothing, got %q", buf.String())
	}
}

func TestEmitTagFooter_KeyValueAndCount(t *testing.T) {
	tags := []store.Tag{
		{ID: "t-aaa", TargetCanonical: aGo, TaggerUUID: "bob", Text: "ETA?", CreatedAt: 100},
		{ID: "t-bbb", TargetCanonical: aGo, TaggerUUID: "carol", Text: "why?", CreatedAt: 200},
	}
	var buf bytes.Buffer
	EmitTagFooter(&buf, tags, "alice")
	got := buf.String()
	if !strings.HasPrefix(got, "ℹ tags count=2 ") {
		t.Errorf("triage first: %s", got)
	}
	if !strings.Contains(got, "owner=alice") {
		t.Errorf("footer must name the lock owner with owner= field: %s", got)
	}
	if strings.Contains(got, "holder=") {
		t.Errorf("holder= field renamed to owner=, must not appear: %s", got)
	}
	if strings.Index(got, "ETA?") > strings.Index(got, "why?") {
		t.Errorf("caller-provided order must be preserved (caller sorts), got:\n%s", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("no ANSI allowed: %q", got)
	}
	// RFC3339 UTC stamp
	if !strings.Contains(got, "at=1970-01-01T00:00:00Z") {
		t.Errorf("RFC3339 UTC stamp missing: %s", got)
	}
}

func TestEmitConflictWithTags_AppendsTagRows(t *testing.T) {
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	tags := map[string][]store.Tag{
		aGo: {{ID: "t-x", TargetCanonical: aGo, TaggerUUID: "bob", Text: "ping", CreatedAt: 0}},
	}
	var buf bytes.Buffer
	EmitConflictWithTags(&buf, &store.MultiConflictError{
		Blockers: []domain.LockRecord{
			{Target: domain.Target{Canonical: aGo}, OwnerUUID: "alice", Intent: "x", ExpiresAt: now},
		},
	}, tags)
	got := buf.String()
	if !strings.HasPrefix(got, "✗ blocked count=1\n") {
		t.Errorf("triage first: %s", got)
	}
	if !strings.Contains(got, "ℹ   tag id=t-x") {
		t.Errorf("indented tag row missing: %s", got)
	}
	if !strings.Contains(got, `text="ping"`) {
		t.Errorf("text missing: %s", got)
	}
	// Tag row appears AFTER its blocker line.
	if strings.Index(got, "ℹ   tag id=t-x") < strings.Index(got, "⚠ target=") {
		t.Errorf("tag row should follow its blocker line: %s", got)
	}
}

// TestEmitReleaseResults_ReclaimedStale covers the D1 surface (loto-ebkc): a
// plain unlock that reclaimed all-stale foreign holders reports success —
// count-first triage with a distinct reclaimed= field, a ✓ per-row line naming
// the dead owner, exit 0 (reclaim is the desired outcome, not a failure).
func TestEmitReleaseResults_ReclaimedStale(t *testing.T) {
	var buf bytes.Buffer
	exit := EmitReleaseResults(&buf, []store.ReleaseResult{
		{Target: domain.Target{Canonical: aGo}, State: store.StateReclaimedStale, Owner: "DeadOwl"},
		{Target: domain.Target{Canonical: bGo}, State: store.StateUnlocked},
	})
	if exit != 0 {
		t.Errorf("reclaimed-stale → exit 0, got %d", exit)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "✓ unlocked count=1 reclaimed=1\n") {
		t.Errorf("triage line must carry reclaimed= field: %s", got)
	}
	if !strings.Contains(got, "✓ target="+aGo+" state=reclaimed-stale owner=DeadOwl") {
		t.Errorf("reclaimed row must be ✓ with state + dead owner: %s", got)
	}
}

// TestEmitReleaseResults_ReclaimRestoreFailed_KeepsAttribution covers the
// review P3: a reclaim whose chmod restore failed degrades to restore-failed,
// but must not masquerade as the caller's own unlock — it counts under
// reclaimed= (Owner discriminates: only reclaimed rows carry a dead owner into
// restore-failed) and the ⚠ row names that dead owner.
func TestEmitReleaseResults_EmptyInput_EmitsInfoGlyph(t *testing.T) {
	var buf bytes.Buffer
	exit := EmitReleaseResults(&buf, nil)
	if exit != 0 {
		t.Errorf("empty results should exit 0, got %d", exit)
	}
	got := buf.String()
	if strings.HasPrefix(got, "✓") {
		t.Errorf("empty results must NOT use success glyph ✓, got: %s", got)
	}
	if !strings.HasPrefix(got, "ℹ") {
		t.Errorf("empty results should use info glyph ℹ, got: %s", got)
	}
	if !strings.Contains(got, "no locks owned") {
		t.Errorf("empty results should say 'no locks owned', got: %s", got)
	}
}

func TestEmitReleaseResults_EmptySlice_EmitsInfoGlyph(t *testing.T) {
	var buf bytes.Buffer
	exit := EmitReleaseResults(&buf, []store.ReleaseResult{})
	if exit != 0 {
		t.Errorf("empty results should exit 0, got %d", exit)
	}
	got := buf.String()
	if strings.HasPrefix(got, "✓") {
		t.Errorf("empty slice must NOT use success glyph ✓, got: %s", got)
	}
	if !strings.Contains(got, "no locks owned") {
		t.Errorf("empty slice should say 'no locks owned', got: %s", got)
	}
}

func TestEmitClaimSuccess(t *testing.T) {
	var buf bytes.Buffer
	exp := time.Date(2026, 7, 5, 18, 4, 5, 0, time.UTC)
	EmitClaimSuccess(&buf, domain.ClaimRecord{
		PathPrefix: "internal/store",
		CreatedAt:  exp.Add(-4 * time.Hour),
		ExpiresAt:  exp,
	})
	got := buf.String()
	if !strings.HasPrefix(got, "✓ claimed count=1\n") {
		t.Errorf("count-first header: %q", got)
	}
	want := "✓ prefix=internal/store ttl=4h0m0s expires_at=2026-07-05T18:04:05Z\n"
	if !strings.Contains(got, want) {
		t.Errorf("row = %q; want %q", got, want)
	}
	if !strings.Contains(got, "ℹ advisory: claim does not block lock/check") {
		t.Errorf("missing advisory-limit ℹ row: %q", got)
	}
}

func TestEmitClaimRelease_Outcomes(t *testing.T) {
	cases := []struct {
		name     string
		res      store.ClaimReleaseResult
		wantExit int
		wantRows []string
	}{
		{
			name:     "released",
			res:      store.ClaimReleaseResult{PathPrefix: pkgA, State: store.ClaimStateReleased},
			wantExit: 0,
			wantRows: []string{"✓ unclaimed count=1\n", "✓ prefix=pkg/a\n"},
		},
		{
			name:     "no-claim",
			res:      store.ClaimReleaseResult{PathPrefix: pkgA, State: store.ClaimStateNoClaim},
			wantExit: 0,
			wantRows: []string{"✓ unclaimed count=0\n", "ℹ prefix=pkg/a state=no-claim\n"},
		},
		{
			name:     "not-owner",
			res:      store.ClaimReleaseResult{PathPrefix: pkgA, State: store.ClaimStateNotOwner, Owner: "alice"},
			wantExit: 1,
			wantRows: []string{"✓ unclaimed count=0\n", "✗ prefix=pkg/a state=not-owner owner=alice\n"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if exit := EmitClaimRelease(&buf, c.res); exit != c.wantExit {
				t.Errorf("exit=%d; want %d", exit, c.wantExit)
			}
			for _, row := range c.wantRows {
				if !strings.Contains(buf.String(), row) {
					t.Errorf("missing row %q in %q", row, buf.String())
				}
			}
		})
	}
}

func TestEmitClaimsReleased(t *testing.T) {
	t.Run("some released", func(t *testing.T) {
		var buf bytes.Buffer
		EmitClaimsReleased(&buf, []string{"pkg/a", "pkg/b"})
		got := buf.String()
		for _, row := range []string{"ℹ claims-released count=2\n", "✓ prefix=pkg/a\n", "✓ prefix=pkg/b\n"} {
			if !strings.Contains(got, row) {
				t.Errorf("missing row %q in %q", row, got)
			}
		}
	})
	t.Run("none released still emits count", func(t *testing.T) {
		var buf bytes.Buffer
		EmitClaimsReleased(&buf, nil)
		if got := buf.String(); got != "ℹ claims-released count=0\n" {
			t.Errorf("empty claim release = %q, want the count=0 line (never silent)", got)
		}
	})
}

// Both release surfaces name the holder on a not-owner row, matching the
// conflict surface (loto-a8t): the owner id, verbatim.
func TestReleaseNotOwnerNamesHolder(t *testing.T) {
	const uuid = "aaaaaaaa-1111-4111-8111-111111111111"
	want := "owner=" + uuid

	t.Run("lock release", func(t *testing.T) {
		var buf bytes.Buffer
		EmitReleaseResults(&buf, []store.ReleaseResult{
			{Target: domain.Target{Canonical: pkgA}, State: store.StateNotOwner, Owner: uuid},
		})
		if got := buf.String(); !strings.Contains(got, want) {
			t.Errorf("lock not-owner row must name holder %q, got %q", want, got)
		}
	})
	t.Run("claim release", func(t *testing.T) {
		var buf bytes.Buffer
		EmitClaimRelease(&buf, store.ClaimReleaseResult{
			PathPrefix: pkgA, State: store.ClaimStateNotOwner, Owner: uuid,
		})
		if got := buf.String(); !strings.Contains(got, want) {
			t.Errorf("claim not-owner row must name holder %q, got %q", want, got)
		}
	})
}

func TestEmitClaimConflictNamesHolder(t *testing.T) {
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	EmitClaimConflict(&buf, &store.ClaimConflictError{
		Blockers: []domain.ClaimRecord{
			{PathPrefix: "pkg/z", OwnerUUID: ownerRed, Intent: "y", ExpiresAt: now},
			{PathPrefix: pkgA, OwnerUUID: ownerGreen, Intent: "store refactor", ExpiresAt: now},
		},
	})
	got := buf.String()
	if !strings.HasPrefix(got, "✗ blocked count=2\n") {
		t.Errorf("triage first: %q", got)
	}
	if strings.Index(got, "prefix=pkg/a") > strings.Index(got, "prefix=pkg/z") {
		t.Errorf("not sorted by prefix: %q", got)
	}
	if !strings.Contains(got, "blocker=Green") {
		t.Errorf("blocker row must name holder: %q", got)
	}
	if !strings.Contains(got, `intent="store refactor"`) {
		t.Errorf("intent must be quoted: %q", got)
	}
	if !strings.Contains(got, "⚠ prefix=") {
		t.Errorf("per-blocker rows use ⚠: %q", got)
	}
}

// TestEmitConflict_BranchRow pins loto-16cf: a blocker whose record carries a
// branch renders it as a trailing branch= key; an empty branch (pre-16cf row,
// non-git acquire) renders no branch= key at all.
func TestEmitConflict_BranchRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	EmitConflictWithTags(&buf, &store.MultiConflictError{
		Blockers: []domain.LockRecord{
			{Target: domain.Target{Canonical: aGo}, OwnerUUID: ownerGreen, Intent: "x", ExpiresAt: now, Branch: "fix/thing"},
			{Target: domain.Target{Canonical: cGo}, OwnerUUID: ownerRed, Intent: "y", ExpiresAt: now},
		},
	}, nil)
	got := buf.String()
	if !strings.Contains(got, " branch=fix/thing\n") {
		t.Errorf("branch row missing branch= key:\n%s", got)
	}
	if strings.Count(got, "branch=") != 1 {
		t.Errorf("empty-branch row must omit branch= key:\n%s", got)
	}
}
