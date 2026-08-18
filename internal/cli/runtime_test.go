package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
)

// TestPinnedByEnv locks the loto-s3l alignment: identity.PinnedByEnv must mean
// exactly "identity.Ensure resolves a real, lock-owning identity" — the negation
// of "Ensure mints a throwaway". A set-but-EMPTY LOTO_AGENT_ID and a traversal-
// shaped LOTO_SUBAGENT_ID both fall through to a throwaway in Ensure, so both
// read as UNPINNED here. Previously a blank agent id (LookupEnv set=true) and any
// non-empty subagent id wrongly pinned, feeding release --all a throwaway UUID →
// the loto-pody false-success. Post-loto-ai5 this predicate lives beside Ensure
// and dispatches on the SAME classifier, so it can't drift from resolution.
const tcSessID = "sess-1" // representative CLAUDE_CODE_SESSION_ID value

func TestPinnedByEnv(t *testing.T) {
	set := func(s string) *string { return &s } // present-with-value; nil = truly unset
	cases := []struct {
		name       string
		agentID    *string // nil → LOTO_AGENT_ID unset; set("") → present-but-empty (the fleet-dispatcher ephemeral)
		subagentID string
		sessionID  string
		want       bool
	}{
		{"unset-agent-id-unpinned", nil, "", "", false},
		{"blank-agent-id-unpinned", set(""), "", "", false},
		{"agent-id-pins", set("agent-x"), "", "", true},
		{"malformed-subagent-unpinned", nil, "../escape", "", false},
		{"valid-subagent-pins", nil, "sibling-1", "", true},
		{"session-id-pins", nil, "", tcSessID, true},
		// Ensure branches on LOTO_AGENT_ID set-ness BEFORE the session id, so a
		// set-but-empty agent id → throwaway even when a session id is present.
		// The predicate must not let the session leg rescue it (loto-s3l P1).
		{"blank-agent-id-shadows-session", set(""), "", tcSessID, false},
		{"valid-subagent-beats-blank-agent-id", set(""), "sibling-1", tcSessID, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.agentID == nil {
				unsetEnvForTest(t, "LOTO_AGENT_ID")
			} else {
				t.Setenv("LOTO_AGENT_ID", *tc.agentID)
			}
			t.Setenv("LOTO_SUBAGENT_ID", tc.subagentID)
			t.Setenv("CLAUDE_CODE_SESSION_ID", tc.sessionID)
			if got := identity.PinnedByEnv(); got != tc.want {
				t.Errorf("identity.PinnedByEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}

// unsetEnvForTest removes key for the duration of the test, restoring any
// prior value on cleanup — the Unsetenv analogue of t.Setenv (which can only
// set a present value, so it can't express the unset-vs-empty distinction that
// LOTO_AGENT_ID's LookupEnv branch turns on).
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	orig, had := os.LookupEnv(key)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, orig) //nolint:usetesting // restore in a Cleanup closure; t.Setenv is unusable here and can't express the unset case this helper exists for
		} else {
			os.Unsetenv(key)
		}
	})
	os.Unsetenv(key)
}

// TestGitRevParseToplevelCancelled verifies that a cancelled parent context
// short-circuits the git invocation rather than blocking on the subprocess.
// Regression: loto-l6o / gh#51 — hung git left the CLI unkillable.
func TestGitRevParseToplevelCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := gitRevParseToplevel(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancelled exec took too long: %v", time.Since(start))
	}
	// Either the context's Canceled or an exec wrapping it is acceptable; we
	// just need confirmation it wasn't a clean run.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	// Some platforms surface this as an *exec.ExitError after SIGKILL — any
	// non-nil err on a cancelled ctx is acceptable so long as it returned fast.
}

func TestGitTimeoutIsBounded(t *testing.T) {
	if gitTimeout <= 0 || gitTimeout > 30*time.Second {
		t.Fatalf("gitTimeout out of sane range: %v", gitTimeout)
	}
}

// TestIsNotAGitRepo pins the classifier that keeps a real git/infra failure
// from being misreported as "not in a git repo" (adversarial review on
// loto-7wi): only git's own exit-128 "not a git repository" failure reads as
// true; a differently-worded ExitError, a non-ExitError (missing binary,
// ctx timeout), and nil all read as false.
// errGitMissing mimics exec's missing-binary failure — the non-ExitError shape
// that must read false in isNotAGitRepo.
var errGitMissing = errors.New(`exec: "git": executable file not found in $PATH`)

func TestIsNotAGitRepo(t *testing.T) {
	t.Run("real not-a-repo error", func(t *testing.T) {
		t.Chdir(t.TempDir()) // deliberately not a git repo
		_, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--show-toplevel").Output()
		if err == nil {
			t.Fatal("expected git rev-parse to fail in a non-repo tempdir")
		}
		if !isNotAGitRepo(err) {
			t.Fatalf("expected isNotAGitRepo(true) for a real not-a-repo failure, got false; err=%v", err)
		}
	})

	t.Run("other ExitError stderr does not match", func(t *testing.T) {
		exitErr := &exec.ExitError{Stderr: []byte("fatal: some other failure\n")}
		if isNotAGitRepo(exitErr) {
			t.Fatal("expected false for an ExitError whose stderr isn't the not-a-repo message")
		}
	})

	t.Run("non-ExitError reads false", func(t *testing.T) {
		if isNotAGitRepo(errGitMissing) {
			t.Fatal("expected false for a non-ExitError (e.g. missing binary)")
		}
		if isNotAGitRepo(context.DeadlineExceeded) {
			t.Fatal("expected false for a ctx timeout")
		}
	})
}

// An unknown host must never authorize a pid probe. The host-less lock case is
// the one today's `l.Host != r.Host` compare gets wrong: two machines that both
// failed the hostname lookup record Host="", compare equal, and probe pid
// numbers that belong to a different kernel (loto-u7e).
func TestLiveProbeUnknownHostNeverPIDProbes(t *testing.T) {
	rt := &runtime{Ctx: context.Background(), Host: "", HostKnown: false}
	probe := rt.liveProbe()

	for _, tc := range []struct {
		name string
		host string
	}{
		{name: "remote host", host: "otherbox"},
		{name: "host-less record", host: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probe(domain.LockRecord{Host: tc.host, PID: os.Getpid()})
			if got != domain.LivenessUnknown {
				t.Errorf("liveProbe(host=%q) = %v, want %v", tc.host, got, domain.LivenessUnknown)
			}
		})
	}
}

// --- openRuntime / openRuntimeGC verb split (loto-6pn6) ---------------------

// agentFilePath returns the on-disk path of uuid's agent record under the
// current $HOME.
func agentFilePath(uuid string) string {
	return filepath.Join(os.Getenv("HOME"), ".loto", "agents", uuid+".json")
}

// mintUnboundAgent mints a fresh, persisted, UNPINNED agent record — no
// LOTO_AGENT_ID and no session cache references it — so backdating its file
// is the only thing standing between it and GC.
func mintUnboundAgent(t *testing.T) *identity.Agent {
	t.Helper()
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	os.Unsetenv("LOTO_SUBAGENT_ID")
	a, err := identity.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func backdate(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// gcVerbSplitFixture sets up one live lock (owned by a pinned "holder"
// identity, via a real `loto lock` invocation) plus one unpinned agent
// record backdated past agentsGCMaxAge — the shared fixture for
// TestOpenRuntimeDoesNotReapAgents / TestOpenRuntimeGCReapsAgents. Restores
// LOTO_AGENT_ID to holder afterward (mintUnboundAgent unset it to mint the
// orphan cleanly) so the caller's own openRuntime/openRuntimeGC call
// resolves the same identity that owns the lock, matching production shape.
func gcVerbSplitFixture(t *testing.T) (holder *identity.Agent, orphanPath string) {
	t.Helper()
	withTempProject(t)
	holder = pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock setup failed, exit %d", code)
	}

	orphan := mintUnboundAgent(t)
	orphanPath = agentFilePath(orphan.UUID)
	backdate(t, orphanPath, 90*24*time.Hour)

	t.Setenv("LOTO_AGENT_ID", holder.UUID)
	return holder, orphanPath
}

// TestOpenRuntimeDoesNotReapAgents is the perf fix expressed as a behavioral
// assertion, not a benchmark: openRuntime is the read path (check, check
// --gate, guard, status) and must never run identity GC (loto-6pn6).
func TestOpenRuntimeDoesNotReapAgents(t *testing.T) {
	_, orphanPath := gcVerbSplitFixture(t)

	rt, err := openRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("openRuntime must not GC agents on the read path; orphan gone: %v", err)
	}
}

// TestOpenRuntimeGCReapsAgents is TestOpenRuntimeDoesNotReapAgents' pair:
// same fixture, openRuntimeGC → the unpinned stale record is gone.
// identity.ResetGCOnceForTests clears the process-wide once-per-process GC
// guard, which an earlier write-verb test in this binary has near-certainly
// already consumed.
func TestOpenRuntimeGCReapsAgents(t *testing.T) {
	_, orphanPath := gcVerbSplitFixture(t)
	identity.ResetGCOnceForTests()

	rt, err := openRuntimeGC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("openRuntimeGC must reap an unpinned stale agent; err=%v", err)
	}
}

// TestOpenRuntimeGCPinsLockOwners is the gh#125/loto-ffg regression at the
// CLI boundary: a backdated agent record that is the owner of a live lock
// row must survive openRuntimeGC — direct proof the verb split did not drop
// the pin set.
func TestOpenRuntimeGCPinsLockOwners(t *testing.T) {
	withTempProject(t)
	holder := pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock setup failed")
	}
	holderPath := agentFilePath(holder.UUID)
	backdate(t, holderPath, 90*24*time.Hour)

	identity.ResetGCOnceForTests()
	rt, err := openRuntimeGC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	if _, err := os.Stat(holderPath); err != nil {
		t.Fatalf("openRuntimeGC must not reap a live lock's own owner agent (gh#125/loto-ffg): %v", err)
	}
}

// TestMemoLiveProbe pins the per-invocation probe cache (Codex #246): one
// holder is probed once no matter how many (target × record) pairs the gate
// evaluates it for, and records that differ in any field the probe reads stay
// independently probed.
func TestMemoLiveProbe(t *testing.T) {
	const owner = "owner-1"
	calls := 0
	base := domain.LockRecord{Host: "h1", OwnerUUID: owner, PID: 42, ProcStart: 7}
	probe := memoLiveProbe(func(domain.LockRecord) domain.Liveness {
		calls++
		return domain.LivenessAlive
	})

	for range 5 {
		if got := probe(base); got != domain.LivenessAlive {
			t.Fatalf("probe = %v, want alive", got)
		}
	}
	if calls != 1 {
		t.Errorf("repeat probes of one holder = %d calls, want 1", calls)
	}

	// Each field the underlying probe reads must key the cache separately —
	// a collision here would answer for the wrong process.
	distinct := []domain.LockRecord{
		{Host: "h2", OwnerUUID: owner, PID: 42, ProcStart: 7},
		{Host: "h1", OwnerUUID: "owner-2", PID: 42, ProcStart: 7},
		{Host: "h1", OwnerUUID: owner, PID: 43, ProcStart: 7},
		{Host: "h1", OwnerUUID: owner, PID: 42, ProcStart: 8},
	}
	for _, l := range distinct {
		probe(l)
	}
	if want := 1 + len(distinct); calls != want {
		t.Errorf("distinct holders = %d calls, want %d", calls, want)
	}
}

// TestMemoLiveProbe_NilPassesThrough: a nil probe means "no liveness oracle,
// TTL is the sole authority" — wrapping must not conjure a non-nil closure
// that would flip EvalContext.IsStale's nil check.
func TestMemoLiveProbe_NilPassesThrough(t *testing.T) {
	if got := memoLiveProbe(nil); got != nil {
		t.Errorf("memoLiveProbe(nil) = %v, want nil", got)
	}
}
