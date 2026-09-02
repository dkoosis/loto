package identity

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// stubProcStart replaces the start-time reader for one test, so the
// start-time branch of the oracle is exercised deterministically — no real
// recycled pid required.
func stubProcStart(t *testing.T, val int64, ok bool) {
	t.Helper()
	prev := procStartFn
	procStartFn = func(int) (int64, bool) { return val, ok }
	t.Cleanup(func() { procStartFn = prev })
}

// deadPID starts and reaps a trivial process, then hands back its pid: a pid
// that provably no longer runs. (The kernel could recycle it, but not within a
// test's lifetime on any realistic scheduler.)
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a throwaway process: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	return pid
}

// existingSocket writes a plain file at a short path. The oracle's witness test
// is os.Stat, so any existing file stands in for a listening socket.
func existingSocket(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// procStartStub is one stubbed start-time read for a TestSessionVerdict row.
type procStartStub struct {
	val int64
	ok  bool
}

func TestSessionVerdict(t *testing.T) {
	live := os.Getpid()
	cases := []struct {
		name   string
		rec    SessionRecord
		stub   *procStartStub
		want   SessionLiveness
		reason string
	}{
		{"socket missing → dead", SessionRecord{Socket: filepath.Join(t.TempDir(), "gone.sock"), PID: live}, nil, SessionDead, reasonSocketMissing},
		{"socket only → live", SessionRecord{Socket: existingSocket(t)}, nil, SessionLive, reasonSocketOnly},
		{"nothing recorded → unknown", SessionRecord{}, nil, SessionUnknown, reasonNoWitness},
		{"pid dead → dead", SessionRecord{PID: deadPID(t)}, nil, SessionDead, reasonPIDDead},
		{"pid alive, no start-time → live", SessionRecord{PID: live}, nil, SessionLive, reasonPID},
		{"socket + pid alive → live", SessionRecord{Socket: existingSocket(t), PID: live}, nil, SessionLive, "socket+" + reasonPID},
		{"start-time match → live", SessionRecord{PID: live, ProcStart: 100}, &procStartStub{100, true}, SessionLive, reasonProcStart},
		{"socket + start-time match → live", SessionRecord{Socket: existingSocket(t), PID: live, ProcStart: 100}, &procStartStub{100, true}, SessionLive, "socket+" + reasonProcStart},
		{"start-time mismatch → dead (recycled pid)", SessionRecord{PID: live, ProcStart: 100}, &procStartStub{101, true}, SessionDead, reasonProcStartMismatch},
		{"start-time unreadable → no signal, live on pid", SessionRecord{PID: live, ProcStart: 100}, &procStartStub{0, false}, SessionLive, reasonPID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.stub != nil {
				stubProcStart(t, tc.stub.val, tc.stub.ok)
			}
			got := tc.rec.Verdict()
			if got.Liveness != tc.want || got.Reason != tc.reason {
				t.Errorf("verdict = %s/%s, want %s/%s", got.Liveness, got.Reason, tc.want, tc.reason)
			}
			if got.Record == nil {
				t.Error("verdict must carry the record it judged")
			}
		})
	}
}

// TestProbeSessionWithoutRecord: no file → unknown, and the caller's own
// degraded signal governs. Never dead.
func TestProbeSessionWithoutRecord(t *testing.T) {
	clearIdentityEnv(t)
	v := ProbeSession(tcSessionA)
	if v.Liveness != SessionUnknown || v.Reason != reasonNoRecord || v.Record != nil {
		t.Errorf("verdict = %+v", v)
	}
	if v := ProbeSession("../escape"); v.Liveness != SessionUnknown {
		t.Errorf("traversal-shaped sid must read unknown, got %s", v.Liveness)
	}
}

// TestProbeSessionReadsWhatRecordSessionWrote is the round trip the lock
// liveness probe depends on: whoami records the live session pid, and a later
// process asking about that session reads it back as live; kill the witness
// and the same record reads dead.
func TestProbeSessionReadsWhatRecordSessionWrote(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)
	t.Setenv("LOTO_PID", "0")
	unsetEnv(t, "CLAUDE_PID")
	socket := existingSocket(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)

	if _, err := RecordSession(&Agent{UUID: tcSessionA}); err != nil {
		t.Fatal(err)
	}
	if v := ProbeSession(tcSessionA); v.Liveness != SessionLive {
		t.Errorf("live session read %s (%s)", v.Liveness, v.Reason)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	if v := ProbeSession(tcSessionA); v.Liveness != SessionDead || v.Reason != reasonSocketMissing {
		t.Errorf("session whose socket vanished read %s (%s)", v.Liveness, v.Reason)
	}
}

// TestProbeSessionDoesNotPruneDeadRecord: the oracle is a pure observer. A
// DEAD answer must leave the file in place — a session restarting inside the
// verdict window republishes its record, and an unlink here would delete the
// fresh one (the loto-gj1z race, one layer up).
func TestProbeSessionDoesNotPruneDeadRecord(t *testing.T) {
	clearIdentityEnv(t)
	path := plantSession(t, tcSessionA, tcSessionA, 0)
	body := `{"session_id":"` + tcSessionA + `","uuid":"` + tcSessionA + `","socket":"` + filepath.Join(t.TempDir(), "gone.sock") + `"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := ProbeSession(tcSessionA); v.Liveness != SessionDead {
		t.Fatalf("want dead, got %s", v.Liveness)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("oracle must not unlink the record it judged: %v", err)
	}
}

func TestSessionLivenessString(t *testing.T) {
	for s, want := range map[SessionLiveness]string{SessionLive: "live", SessionDead: "dead", SessionUnknown: livenessUnknownName, SessionLiveness(99): livenessUnknownName} {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(s), got, want)
		}
	}
}

func TestPIDAlive(t *testing.T) {
	if !PIDAlive(os.Getpid()) {
		t.Error("own pid must be alive")
	}
	if PIDAlive(deadPID(t)) {
		t.Error("reaped pid must be dead")
	}
	if PIDAlive(0) || PIDAlive(-1) {
		t.Error("non-positive pids are never alive")
	}
}
