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

const aGo = "a.go"

var errPermissionDenied error = permDeniedError{}

type permDeniedError struct{}

func (permDeniedError) Error() string { return "permission denied" }

func TestEmitLockSuccess_SortedDeterministic(t *testing.T) {
	var buf bytes.Buffer
	EmitLockSuccess(&buf, []domain.Target{
		{Canonical: "z.go"},
		{Canonical: aGo},
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

func TestEmitConflict_TriageFirst(t *testing.T) {
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	EmitConflict(&buf, &store.MultiConflictError{
		Blockers: []domain.LockRecord{
			{Target: domain.Target{Canonical: aGo}, OwnerUUID: "Green", Intent: "x", ExpiresAt: now},
			{Target: domain.Target{Canonical: "c.go"}, OwnerUUID: "Red", Intent: "y", ExpiresAt: now},
		},
	})
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

func TestEmitReleaseResults_MixedOutcomes(t *testing.T) {
	var buf bytes.Buffer
	exit := EmitReleaseResults(&buf, []store.ReleaseResult{
		{Target: domain.Target{Canonical: aGo}, State: store.StateUnlocked},
		{Target: domain.Target{Canonical: "b.go"}, State: store.StateNoLock},
		{Target: domain.Target{Canonical: "c.go"}, State: store.StateNotOwner, Holder: "BlueOak"},
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
			{Target: domain.Target{Canonical: "b.go"}, RolledBack: true},
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
