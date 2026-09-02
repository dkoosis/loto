package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
const (
	tcSessID  = "sess-1"    // representative CLAUDE_CODE_SESSION_ID value
	tcSibling = "sibling-1" // representative LOTO_SUBAGENT_ID stamp
)

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
		{"valid-subagent-pins", nil, tcSibling, tcSessID, true},
		// A stamp derives its owner FROM the session id, so without one there
		// is nothing to derive from and it falls open (loto-jnid).
		{"subagent-without-session-unpinned", nil, tcSibling, "", false},
		{"session-id-pins", nil, "", tcSessID, true},
		// Ensure branches on LOTO_AGENT_ID set-ness BEFORE the session id, so a
		// set-but-empty agent id → throwaway even when a session id is present.
		// The predicate must not let the session leg rescue it (loto-s3l P1).
		{"blank-agent-id-shadows-session", set(""), "", tcSessID, false},
		{"valid-subagent-beats-blank-agent-id", set(""), tcSibling, tcSessID, true},
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

// --- openRuntime / openRuntimeGC verb split (loto-6pn6, loto-jnid) ---------

// TestOpenRuntimeGCRefusesUnpinned is the authority half of "ambiguity allowed
// for display, never for authority": a write verb with nothing in the env
// naming an owner must refuse before touching the store, naming the env vars
// that would fix it. Read verbs (openRuntime) keep working on a throwaway.
func TestOpenRuntimeGCRefusesUnpinned(t *testing.T) {
	withTempProject(t)
	unsetEnvForTest(t, "LOTO_AGENT_ID")
	unsetEnvForTest(t, "CLAUDE_CODE_SESSION_ID")
	unsetEnvForTest(t, "LOTO_SUBAGENT_ID")

	if _, err := openRuntimeGC(context.Background()); !errors.Is(err, identity.ErrUnpinned) {
		t.Fatalf("openRuntimeGC unpinned: err=%v, want identity.ErrUnpinned", err)
	}

	rt, err := openRuntime(context.Background())
	if err != nil {
		t.Fatalf("openRuntime unpinned must still open on a throwaway: %v", err)
	}
	defer rt.Close()
	if rt.AgentPinned {
		t.Error("openRuntime unpinned: AgentPinned = true, want false — a throwaway must never scope a release --all")
	}
	if rt.Agent == nil || rt.Agent.UUID == "" {
		t.Errorf("openRuntime unpinned: Agent = %+v, want a throwaway id for display", rt.Agent)
	}
}

// TestLockRefusesUnpinned is the same rule at the CLI surface: `loto lock` on a
// bare shell exits 3 with one ✗ line naming CLAUDE_CODE_SESSION_ID and
// LOTO_AGENT_ID, and writes nothing.
func TestLockRefusesUnpinned(t *testing.T) {
	withTempProject(t)
	unsetEnvForTest(t, "LOTO_AGENT_ID")
	unsetEnvForTest(t, "CLAUDE_CODE_SESSION_ID")
	unsetEnvForTest(t, "LOTO_SUBAGENT_ID")

	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &out, &errBuf); code != 3 {
		t.Fatalf("unpinned lock exit %d, want 3; out=%q err=%q", code, out.String(), errBuf.String())
	}
	for _, want := range []string{"✗", "CLAUDE_CODE_SESSION_ID", "LOTO_AGENT_ID"} {
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("refusal must carry %q: %q", want, errBuf.String())
		}
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "observer-"+tcSessID)
	var status bytes.Buffer
	if code := Run([]string{"status"}, &status, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit %d", code)
	}
	if strings.Contains(status.String(), tcTargetA) {
		t.Errorf("a refused lock must not leave a row behind: %q", status.String())
	}
}

// TestLockOwnerIsTheSessionID pins the ownership contract (loto-jnid): a lock
// taken under one CLAUDE_CODE_SESSION_ID is owned by that id verbatim, a
// second session is refused, and `loto check` names the holder by it.
func TestLockOwnerIsTheSessionID(t *testing.T) {
	withTempProject(t)
	unsetEnvForTest(t, "LOTO_AGENT_ID")
	unsetEnvForTest(t, "LOTO_SUBAGENT_ID")
	const (
		sessA = "aaaaaaaa-0000-4000-8000-00000000000a"
		sessB = "bbbbbbbb-0000-4000-8000-00000000000b"
	)
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessA)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid())) // a live holder, so check hard-blocks
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("session A lock exit %d", code)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", sessB)
	var out bytes.Buffer
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &out, &bytes.Buffer{}); code != 1 {
		t.Fatalf("session B lock exit %d, want 1 (conflict); out=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "blocker="+sessA) {
		t.Errorf("conflict row must name the holder by session id %q: %q", sessA, out.String())
	}
	out.Reset()
	if code := Run([]string{"check", tcTargetA}, &out, &bytes.Buffer{}); code != 1 {
		t.Fatalf("check exit %d, want 1; out=%q", code, out.String())
	}
	if !strings.Contains(out.String(), sessA) {
		t.Errorf("check must name the holder by session id %q: %q", sessA, out.String())
	}

	// Re-entrant from the owning session: the same id is the same owner.
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessA)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("session A re-lock exit %d, want 0 (re-entrant)", code)
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

// writeDeadSession plants a session record for sid whose socket path does not
// exist. SessionRecord.Verdict's first probe is a stat of that socket, so the
// record reads DEAD with reason=socket-missing — the cheapest honest way to
// stage "one sibling session has ended" without killing a real process.
func writeDeadSession(t *testing.T, sid, owner string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".loto", "session")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(
		`{"session_id":%q,"uuid":%q,"socket":%q,"recorded_at":%q}`,
		sid, owner, filepath.Join(t.TempDir(), "gone.sock"), time.Now().Format(time.RFC3339),
	)
	if err := os.WriteFile(filepath.Join(dir, sid+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Sibling sessions sharing one LOTO_AGENT_ID are supported (loto-81n), so one
// owner uuid can have several live sessions. The oracle is keyed on the lock
// row's SESSION id, not its owner: a dead sibling's record condemns only the
// locks that sibling's session took. Before loto-2lj5 the verdict was handed
// back for every lock the owner held — classifying live siblings' locks stale,
// which lets two agents write one file.
func TestLiveProbeDeadSiblingDoesNotCondemnLiveSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unsetEnvForTest(t, "LOTO_BASE")
	const (
		agentUUID = "11111111-2222-3333-4444-555555555555"
		deadSess  = "session-1-that-died"
		liveSess  = "session-2-still-running"
	)
	writeDeadSession(t, deadSess, agentUUID)

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
		t.Errorf("lock held by the session whose record died: got %v, want %v", got, domain.LivenessDead)
	}
	if got := probe(rec(liveSess)); got == domain.LivenessDead {
		t.Errorf("lock held by a DIFFERENT session of the same owner: got %v, want anything but dead — a sibling's death is no evidence about this holder", got)
	}
	if got := probe(rec("")); got == domain.LivenessDead {
		t.Errorf("legacy row with no session id: got %v, want anything but dead — nothing to look up, TTL governs", got)
	}
}

// The oracle above is only meaningful if a record's SessionUUID is actually the
// session's id. It was not: nothing has ever exported LOTO_SESSION_ID, so
// sessionUUID minted a fresh UUID per INVOCATION and two locks taken by one
// Claude session carried different ids. CLAUDE_CODE_SESSION_ID is the id
// identity.RecordSession keys the session record by, so sourcing it here is
// what makes the two sides comparable by construction (loto-2lj5).
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
