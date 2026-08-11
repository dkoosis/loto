package identity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	oracleUUID   = "aaaaaaaa-2222-4333-8444-555555555555"
	oracleHandle = "OracleTest"
	agentIDX     = "x@team"
	agentIDOther = "other@team"
	parentSessA  = "sess-a"
	parentSessB  = "sess-b"
	topLevelArgv = "claude --permission-mode auto"

	// The oracle's machine-stable reason tokens, named here so a rename in
	// liveness.go shows up as one compile-time diff rather than scattered strings.
	reasonNoSocket      = "no-socket-recorded"
	reasonSocketMissing = "socket-missing"
	reasonSocketOnly    = "socket-only"
	reasonPIDDead       = "pid-dead"
	reasonSocketPID     = "socket+pid"
	reasonPSMatch       = "socket+ps-match"
	reasonArgvMismatch  = "argv-mismatch"
	reasonNoRecord      = "no-peer-record"

	livenessUnknown = "unknown"
)

// stubArgv replaces the proc-table read for one test, so the argv branch of the
// oracle is exercised without a real process carrying the flags.
func stubArgv(t *testing.T, argv string) {
	t.Helper()
	prev := procArgv
	procArgv = func(context.Context, int) string { return argv }
	t.Cleanup(func() { procArgv = prev })
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
// is os.Stat, so any existing file stands in for a listening socket; peer_test's
// liveSocket covers the real-listener shape.
func existingSocket(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSessionVerdict(t *testing.T) {
	sock := existingSocket(t)
	gone := filepath.Join(t.TempDir(), "gone.sock")
	self := os.Getpid()
	dead := deadPID(t)

	tests := []struct {
		name         string
		socket       string
		pid          int
		agentID      string
		parentSess   string
		argv         string
		wantLiveness SessionLiveness
		wantReason   string
	}{
		{
			name:         "no socket recorded",
			socket:       "",
			pid:          self,
			wantLiveness: SessionUnknown,
			wantReason:   reasonNoSocket,
		},
		{
			name:         "socket path gone",
			socket:       gone,
			pid:          self,
			wantLiveness: SessionDead,
			wantReason:   reasonSocketMissing,
		},
		{
			name:         "socket without a pid",
			socket:       sock,
			pid:          0,
			wantLiveness: SessionLive,
			wantReason:   reasonSocketOnly,
		},
		{
			name:         "pid reaped",
			socket:       sock,
			pid:          dead,
			wantLiveness: SessionDead,
			wantReason:   reasonPIDDead,
		},
		{
			name:         "proc table unreadable",
			socket:       sock,
			pid:          self,
			agentID:      agentIDX,
			argv:         "",
			wantLiveness: SessionLive,
			wantReason:   reasonSocketPID,
		},
		{
			name:         "argv carries the recorded agent id",
			socket:       sock,
			pid:          self,
			agentID:      agentIDX,
			argv:         "claude --agent-id " + agentIDX,
			wantLiveness: SessionLive,
			wantReason:   reasonPSMatch,
		},
		{
			name:         "argv equals form",
			socket:       sock,
			pid:          self,
			agentID:      agentIDX,
			argv:         "claude --agent-id=" + agentIDX,
			wantLiveness: SessionLive,
			wantReason:   reasonPSMatch,
		},
		{
			name:         "argv carries a different agent id",
			socket:       sock,
			pid:          self,
			agentID:      agentIDX,
			argv:         "claude --agent-id " + agentIDOther,
			wantLiveness: SessionDead,
			wantReason:   reasonArgvMismatch,
		},
		{
			name:         "recycled pid dropped the agent-id flag",
			socket:       sock,
			pid:          self,
			agentID:      agentIDX,
			argv:         topLevelArgv,
			wantLiveness: SessionDead,
			wantReason:   reasonArgvMismatch,
		},
		{
			name:         "top-level session recorded no flags",
			socket:       sock,
			pid:          self,
			argv:         "some --other process --agent-id " + agentIDOther,
			wantLiveness: SessionLive,
			wantReason:   reasonPSMatch,
		},
		{
			name:         "parent session id matches",
			socket:       sock,
			pid:          self,
			parentSess:   parentSessA,
			argv:         "claude --parent-session-id " + parentSessA,
			wantLiveness: SessionLive,
			wantReason:   reasonPSMatch,
		},
		{
			name:         "parent session id differs",
			socket:       sock,
			pid:          self,
			parentSess:   parentSessA,
			argv:         "claude --parent-session-id " + parentSessB,
			wantLiveness: SessionDead,
			wantReason:   reasonArgvMismatch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubArgv(t, tc.argv)
			p := &Peer{
				UUID:            oracleUUID,
				Handle:          oracleHandle,
				Socket:          tc.socket,
				PID:             tc.pid,
				AgentID:         tc.agentID,
				ParentSessionID: tc.parentSess,
			}
			got := p.SessionVerdict(context.Background())
			if got.Liveness != tc.wantLiveness || got.Reason != tc.wantReason {
				t.Fatalf("SessionVerdict = %s/%s, want %s/%s",
					got.Liveness, got.Reason, tc.wantLiveness, tc.wantReason)
			}
			// Value receiver: the verdict carries a copy of the record it
			// consulted, so identity is by content, not pointer.
			if got.Peer == nil || got.Peer.UUID != p.UUID || got.Peer.PID != p.PID {
				t.Fatalf("verdict must carry the record it consulted; got %+v", got.Peer)
			}
			// Live() is the boolean face of the same oracle: unknown is not dead.
			if want := tc.wantLiveness != SessionDead; p.Live(context.Background()) != want {
				t.Fatalf("Live() = %v, want %v for verdict %s", p.Live(context.Background()), want, got.Liveness)
			}
		})
	}
}

func TestSessionLivenessString(t *testing.T) {
	tests := []struct {
		liveness SessionLiveness
		want     string
	}{
		{SessionLive, "live"},
		{SessionDead, "dead"},
		{SessionUnknown, livenessUnknown},
		{SessionLiveness(99), livenessUnknown},
	}
	for _, tc := range tests {
		if got := tc.liveness.String(); got != tc.want {
			t.Errorf("SessionLiveness(%d).String() = %q, want %q", tc.liveness, got, tc.want)
		}
	}
}

func TestPIDAlive(t *testing.T) {
	if PIDAlive(0) || PIDAlive(-1) {
		t.Error("a non-positive pid is never alive")
	}
	if !PIDAlive(os.Getpid()) {
		t.Error("the test process must read alive")
	}
	if PIDAlive(deadPID(t)) {
		t.Error("a reaped child must read dead")
	}
}

func TestAgentLiveWithoutRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := []struct {
		name string
		uuid string
	}{
		{"no record on disk", oracleUUID},
		{"unusable uuid", "../../etc/passwd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AgentLive(context.Background(), tc.uuid)
			if got.Liveness != SessionUnknown || got.Reason != reasonNoRecord {
				t.Fatalf("AgentLive = %s/%s, want unknown/no-peer-record", got.Liveness, got.Reason)
			}
			if got.Peer != nil {
				t.Fatalf("no record means no peer to carry; got %+v", got.Peer)
			}
		})
	}
}

func TestAgentLiveDelegatesToSessionVerdict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sock := existingSocket(t)
	stubArgv(t, "claude --agent-id "+agentIDX)

	p := &Peer{
		UUID:    oracleUUID,
		Handle:  oracleHandle,
		Socket:  sock,
		PID:     os.Getpid(),
		AgentID: agentIDX,
		SeenAt:  time.Now().UTC(),
	}
	if err := writePeer(p); err != nil {
		t.Fatal(err)
	}

	got := AgentLive(context.Background(), oracleUUID)
	if got.Liveness != SessionLive || got.Reason != reasonPSMatch {
		t.Fatalf("AgentLive = %s/%s, want live/socket+ps-match", got.Liveness, got.Reason)
	}
	if got.Peer == nil || got.Peer.Handle != oracleHandle {
		t.Fatalf("verdict must carry the record read from disk; got %+v", got.Peer)
	}

	// Same record, socket now gone: the oracle reads the death.
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	if got = AgentLive(context.Background(), oracleUUID); got.Liveness != SessionDead || got.Reason != reasonSocketMissing {
		t.Fatalf("AgentLive = %s/%s, want dead/socket-missing", got.Liveness, got.Reason)
	}
}

// The oracle is a pure observer: asking whether an agent is live must never
// unlink its record, or a liveness question asked mid-restart deletes the fresh
// record (the gj1z race). Pruning is Peers()' job alone.
func TestAgentLiveDoesNotPruneDeadRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubArgv(t, topLevelArgv)

	p := &Peer{
		UUID:   oracleUUID,
		Handle: oracleHandle,
		Socket: filepath.Join(t.TempDir(), "gone.sock"), // never created
		PID:    os.Getpid(),
		SeenAt: time.Now().UTC(),
	}
	if err := writePeer(p); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(home, ".loto", "peers", oracleUUID+".json")

	if got := AgentLive(context.Background(), oracleUUID); got.Liveness != SessionDead {
		t.Fatalf("AgentLive = %s, want dead", got.Liveness)
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("AgentLive must not unlink a dead record: stat err = %v", err)
	}

	// Contrast: listing peers is the pruning path, and it does remove it.
	peers, err := Peers(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Fatalf("Peers(false) = %+v, want empty", peers)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("Peers must prune the dead record; stat err = %v", err)
	}
}
