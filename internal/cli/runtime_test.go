package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	const session = "session-1"
	calls := 0
	base := domain.LockRecord{Host: "h1", OwnerUUID: owner, SessionUUID: session, PID: 42, ProcStart: 7}
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
		{Host: "h2", OwnerUUID: owner, SessionUUID: session, PID: 42, ProcStart: 7},
		{Host: "h1", OwnerUUID: "owner-2", SessionUUID: session, PID: 42, ProcStart: 7},
		{Host: "h1", OwnerUUID: owner, SessionUUID: session, PID: 43, ProcStart: 7},
		{Host: "h1", OwnerUUID: owner, SessionUUID: session, PID: 42, ProcStart: 8},
		// loto-s0bb: the session is a field the probe reads (peerSpeaksFor), so
		// it must key the cache too.
		{Host: "h1", OwnerUUID: owner, SessionUUID: "session-2", PID: 42, ProcStart: 7},
	}
	for _, l := range distinct {
		probe(l)
	}
	if want := 1 + len(distinct); calls != want {
		t.Errorf("distinct holders = %d calls, want %d", calls, want)
	}
}

// TestMemoLiveProbe_SiblingSessionsDoNotShareAVerdict is the loto-s0bb
// regression pin (Codex #248). Sibling sessions of one agent share an owner
// uuid, and every sibling beacon and every claim carries PID 0 — so on the old
// key (host, owner, pid, proc-start) two sibling records were one cache entry.
// liveProbe answers DEAD only for the session the single peer record names and
// falls through to UNKNOWN for the others, so probing the dead sibling first
// served its DEAD verdict to a live one: check --gate, guard, and claim
// acquisition would then hand a live agent's territory to a competing writer.
func TestMemoLiveProbe_SiblingSessionsDoNotShareAVerdict(t *testing.T) {
	const owner = "shared-agent"
	dead := domain.LockRecord{Host: "h1", OwnerUUID: owner, SessionUUID: "dead-session"}
	live := domain.LockRecord{Host: "h1", OwnerUUID: owner, SessionUUID: "live-session"}

	probe := memoLiveProbe(func(l domain.LockRecord) domain.Liveness {
		if l.SessionUUID == dead.SessionUUID {
			return domain.LivenessDead
		}
		return domain.LivenessUnknown
	})

	// Dead sibling first — the order that poisoned the cache.
	if got := probe(dead); got != domain.LivenessDead {
		t.Fatalf("dead sibling = %v, want dead", got)
	}
	if got := probe(live); got != domain.LivenessUnknown {
		t.Fatalf("live sibling = %v, want unknown — a dead sibling's verdict was cached for it", got)
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

// --- loto-2lj5: a dead sibling must not condemn a live sibling's lock -------

// writeDeadPeer plants a peer record for uuid whose socket path does not exist.
// SessionVerdict's first probe is a stat of that socket, so the record reads
// DEAD with reason=socket-missing — the cheapest honest way to stage "one
// sibling session has ended" without killing a real process.
func writeDeadPeer(t *testing.T, uuid, sessionID string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".loto", "peers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(
		`{"uuid":%q,"handle":"sibling-1","session_id":%q,"socket":%q,"seen_at":%q}`,
		uuid, sessionID, filepath.Join(t.TempDir(), "gone.sock"), time.Now().Format(time.RFC3339),
	)
	if err := os.WriteFile(filepath.Join(dir, uuid+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Peer records are keyed on agent uuid alone (identity.peerPath), but sibling
// sessions sharing one LOTO_AGENT_ID are supported (loto-81n). So one dead
// sibling leaves one DEAD peer record standing for every live sibling too, and
// before loto-2lj5 the oracle handed that verdict straight back for locks the
// live siblings hold — classifying them stale, which lets two agents write one
// file. The gate: a DEAD verdict counts only for the session it names.
func TestLiveProbeDeadSiblingDoesNotCondemnLiveSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const (
		agentUUID = "11111111-2222-3333-4444-555555555555"
		deadSess  = "session-1-that-died"
		liveSess  = "session-2-still-running"
	)
	writeDeadPeer(t, agentUUID, deadSess)

	rt := &runtime{Ctx: context.Background(), Host: "testhost", HostKnown: true}
	probe := rt.liveProbe()
	// PID 0 is the claim-shaped sentinel: no pid witness, so whatever the
	// oracle leg decides is the whole verdict. That isolates the gate.
	rec := func(session string) domain.LockRecord {
		return domain.LockRecord{
			Host:        "testhost",
			OwnerUUID:   domain.AgentUUID(agentUUID),
			SessionUUID: domain.SessionUUID(session),
		}
	}

	if got := probe(rec(deadSess)); got != domain.LivenessDead {
		t.Errorf("lock held by the session the peer record names: got %v, want %v — gating the verdict must not disable reclaim for the session that actually died", got, domain.LivenessDead)
	}
	if got := probe(rec(liveSess)); got == domain.LivenessDead {
		t.Errorf("lock held by a DIFFERENT session of the same agent: got %v, want anything but dead — a sibling's death is no evidence about this holder, and a false dead lets two agents write one file", got)
	}
}

// peerSpeaksFor is the whole gate, so pin its edges directly. The both-empty
// case is load-bearing: direct CLI use and legacy rows carry no session id on
// either side, and reclaim must keep working there rather than silently
// falling back to TTL for every pre-existing lock.
func TestPeerSpeaksFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		peer   *identity.Peer
		record domain.SessionUUID
		want   bool
	}{
		{"no peer record", nil, "s1", false},
		{"same session", &identity.Peer{SessionID: "s1"}, "s1", true},
		{"different session", &identity.Peer{SessionID: "s1"}, "s2", false},
		{"both empty — not a Claude session on either side", &identity.Peer{}, "", true},
		{"peer has none, record does", &identity.Peer{}, "s1", false},
		{"record has none, peer does", &identity.Peer{SessionID: "s1"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := peerSpeaksFor(tc.peer, tc.record); got != tc.want {
				t.Errorf("peerSpeaksFor = %v, want %v", got, tc.want)
			}
		})
	}
}

// The gate above is only meaningful if a record's SessionUUID is actually the
// session's id. It was not: nothing has ever exported LOTO_SESSION_ID, so
// sessionUUID minted a fresh UUID per INVOCATION and two locks taken by one
// Claude session carried different ids. CLAUDE_CODE_SESSION_ID is the id
// identity.RecordPeer already writes into Peer.SessionID, so sourcing it here
// is what makes the two sides comparable by construction (loto-2lj5).
func TestSessionUUIDSourcesTheClaudeSessionID(t *testing.T) {
	t.Run("explicit override wins", func(t *testing.T) {
		t.Setenv("LOTO_SESSION_ID", "explicit")
		t.Setenv("CLAUDE_CODE_SESSION_ID", "cc-session")
		id, pinned := sessionUUID()
		if id != "explicit" || !pinned {
			t.Errorf("sessionUUID = (%q, %v), want (explicit, true)", id, pinned)
		}
	})

	t.Run("claude session id pins and is stable across calls", func(t *testing.T) {
		os.Unsetenv("LOTO_SESSION_ID")
		t.Setenv("CLAUDE_CODE_SESSION_ID", "cc-session")
		first, pinned := sessionUUID()
		if first != "cc-session" || !pinned {
			t.Errorf("sessionUUID = (%q, %v), want (cc-session, true)", first, pinned)
		}
		if second, _ := sessionUUID(); second != first {
			t.Errorf("two invocations in one session got %q then %q; every shell-out from one session must share an id (records.go SessionUUID contract)", first, second)
		}
	})

	t.Run("neither set mints an unpinned throwaway", func(t *testing.T) {
		os.Unsetenv("LOTO_SESSION_ID")
		os.Unsetenv("CLAUDE_CODE_SESSION_ID")
		id, pinned := sessionUUID()
		if id == "" || pinned {
			t.Errorf("sessionUUID = (%q, %v), want (a fresh uuid, false) — an unpinned id must never be used as a release filter (loto-pody)", id, pinned)
		}
	})
}
