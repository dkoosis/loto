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
	aGo = "a.go"
	bGo = "b.go"
	cGo = "c.go"
)

var errPermissionDenied error = permDeniedError{}

type permDeniedError struct{}

func (permDeniedError) Error() string { return "permission denied" }

var errAuditWriteFailed error = auditWriteError{}

type auditWriteError struct{}

func (auditWriteError) Error() string { return "audit-write failed: database is closed" }

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
			{Target: domain.Target{Canonical: aGo}, OwnerUUID: "Green", Intent: "x", ExpiresAt: now},
			{Target: domain.Target{Canonical: cGo}, OwnerUUID: "Red", Intent: "y", ExpiresAt: now},
		},
	}, nil)
	got := buf.String()
	if !strings.HasPrefix(got, "✗ blocked count=2\n") {
		t.Errorf("triage first: %s", got)
	}
}

func TestHolderTag_FallsBackToUUIDWhenUnknown(t *testing.T) {
	// HOME points to an empty dir → registry lookup returns ErrNotExist →
	// holderTag returns the bare UUID.
	t.Setenv("HOME", t.TempDir())
	uuid := "00000000-0000-0000-0000-000000000000"
	if got := holderTag(uuid); got != uuid {
		t.Errorf("expected fallback to UUID, got %q", got)
	}
}

// TestHolderMemo_DedupsLookups is the N+1 guard: a render pass memoizes
// identity resolution so a repeated UUID triggers exactly one underlying
// lookup (ReadFile+Unmarshal), not one per row. The memo also caches the
// formatted tag so distinct UUIDs each resolve once.
func TestHolderMemo_DedupsLookups(t *testing.T) {
	calls := map[string]int{}
	m := &holderMemo{resolve: func(uuid string) string {
		calls[uuid]++
		return "H(" + uuid + ")"
	}}
	// Same UUID three times → one resolve. A second distinct UUID → one resolve.
	for range 3 {
		if got := m.tag("alice"); got != "H(alice)" {
			t.Fatalf("tag(alice) = %q, want H(alice)", got)
		}
	}
	m.tag("bob")
	if calls["alice"] != 1 {
		t.Errorf("alice resolved %d times, want 1 (memo miss = N+1 regression)", calls["alice"])
	}
	if calls["bob"] != 1 {
		t.Errorf("bob resolved %d times, want 1", calls["bob"])
	}
}

// TestHolderMemo_DefaultResolverFallsBack confirms a zero-value memo (nil
// resolve) defaults to the real holderTag logic — bare UUID when unknown.
func TestHolderMemo_DefaultResolverFallsBack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := &holderMemo{}
	uuid := "00000000-0000-0000-0000-000000000000"
	if got := m.tag(uuid); got != uuid {
		t.Errorf("default resolver should fall back to UUID, got %q", got)
	}
}

func TestEmitReleaseResults_MixedOutcomes(t *testing.T) {
	var buf bytes.Buffer
	exit := EmitReleaseResults(&buf, []store.ReleaseResult{
		{Target: domain.Target{Canonical: aGo}, State: store.StateUnlocked},
		{Target: domain.Target{Canonical: bGo}, State: store.StateNoLock},
		{Target: domain.Target{Canonical: cGo}, State: store.StateNotOwner, Holder: "BlueOak"},
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
	if !strings.Contains(got, "holder=BlueOak") {
		t.Errorf("missing holder: %s", got)
	}
}

// TestEmitReleaseResults_SurfacesAuditHole covers loto-vmym's sibling loto-c6rg:
// a restore-failed release whose mode_restore_failed audit event was also lost
// must surface the audit hole to the operator (gh#107), not just the restore
// error. Pre-fix, ReleaseResult.AuditErr was populated but never rendered.
func TestEmitReleaseResults_SurfacesAuditHole(t *testing.T) {
	var buf bytes.Buffer
	exit := EmitReleaseResults(&buf, []store.ReleaseResult{
		{
			Target:     domain.Target{Canonical: aGo},
			State:      store.StateRestoreFailed,
			RestoreErr: errPermissionDenied,
			AuditErr:   errAuditWriteFailed,
		},
	})
	if exit != 1 {
		t.Errorf("restore-failed → exit 1, got %d", exit)
	}
	got := buf.String()
	if !strings.Contains(got, "state=restore-failed") {
		t.Errorf("missing restore-failed state: %s", got)
	}
	if !strings.Contains(got, "audit-hole=") {
		t.Errorf("AuditErr must surface as audit-hole (gh#107): %s", got)
	}
}

// TestEmitReleaseResults_RestoreFailedCountsAsUnlocked covers loto-qv91: a
// restore-failed release deleted the lock row in-tx (a successful unlock) but
// the chmod restore failed. The first-line triage count must count it as
// unlocked, not drop it to 0, while still surfacing the restore failures via a
// distinct restore-failed field. Pre-fix the header read "✓ unlocked count=0"
// for an all-restore-failed slice — actively misleading to the Claude consumer.
func TestEmitReleaseResults_RestoreFailedCountsAsUnlocked(t *testing.T) {
	var buf bytes.Buffer
	exit := EmitReleaseResults(&buf, []store.ReleaseResult{
		{Target: domain.Target{Canonical: aGo}, State: store.StateRestoreFailed, RestoreErr: errPermissionDenied},
		{Target: domain.Target{Canonical: bGo}, State: store.StateRestoreFailed, RestoreErr: errPermissionDenied},
		{Target: domain.Target{Canonical: cGo}, State: store.StateRestoreFailed, RestoreErr: errPermissionDenied},
	})
	if exit != 1 {
		t.Errorf("restore-failed → exit 1, got %d", exit)
	}
	got := buf.String()
	if !strings.Contains(got, "✓ unlocked count=3") {
		t.Errorf("restore-failed rows are unlocked (row deleted), header must report count=3: %s", got)
	}
	if !strings.Contains(got, "restore-failed=3") {
		t.Errorf("restore failures must surface as a distinct first-line field: %s", got)
	}
}

// TestEmitBreakResults_SurfacesRestoreAndAuditHoles covers loto-c6rg on the
// break path: unlock --force results carry RestoreErr/AuditErr that the prior
// inline renderer dropped — a forced break that left a file read-only or lost
// its audit event was silently reported as a clean "✓ broken".
func TestEmitBreakResults_SurfacesRestoreAndAuditHoles(t *testing.T) {
	var out, errBuf bytes.Buffer
	exit := EmitBreakResults(&out, &errBuf, []store.BreakResult{
		{Target: domain.Target{Canonical: aGo}}, // clean break
		{
			Target:     domain.Target{Canonical: bGo},
			RestoreErr: errPermissionDenied,
			AuditErr:   errAuditWriteFailed,
		},
		{Target: domain.Target{Canonical: cGo}, Err: store.ErrNoLockAtTarget},
	})
	if exit != 1 {
		t.Errorf("restore-failed or no-lock → exit 1, got %d", exit)
	}
	if !strings.Contains(out.String(), "✓ broken target=a.go") {
		t.Errorf("clean break must still report success on stdout: %s", out.String())
	}
	gotErr := errBuf.String()
	if !strings.Contains(gotErr, "state=restore-failed") || !strings.Contains(gotErr, "audit-hole=") {
		t.Errorf("break restore-failure + audit hole must surface on stderr (gh#107): %s", gotErr)
	}
	if !strings.Contains(gotErr, "no lock at target=c.go") {
		t.Errorf("missing no-lock line: %s", gotErr)
	}
}

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

func TestEmitChmodFailure_FailedQuotedAndCountsErrOnly(t *testing.T) {
	var buf bytes.Buffer
	EmitChmodFailure(&buf, &store.ChmodFailureError{
		Failures: []store.ChmodFailure{
			{Target: domain.Target{Canonical: aGo}, Err: errPermissionDenied},
			{Target: domain.Target{Canonical: bGo}, RolledBack: true},
		},
	})
	got := buf.String()
	if !strings.HasPrefix(got, "✗ chmod-failed count=1\n") {
		t.Errorf("count should only include rows with Err != nil, got: %s", got)
	}
	if !strings.Contains(got, `err="permission denied"`) {
		t.Errorf("err should be quoted: %s", got)
	}
	if !strings.Contains(got, "state=restored") {
		t.Errorf("missing restored row: %s", got)
	}
}

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
