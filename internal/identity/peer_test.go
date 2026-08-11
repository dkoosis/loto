package identity

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// liveSocket creates a real unix socket so Peer.Live(ctx) sees the same witness it
// sees in production: a file at the socket path. The listener is closed (and
// the file removed) via the returned func to simulate the session exiting.
func liveSocket(t *testing.T, dir string) (path string, kill func()) {
	t.Helper()
	// Unix socket paths are length-capped (~104 bytes on darwin), and
	// t.TempDir() paths are long — keep the basename short.
	path = filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	return path, func() {
		l.Close()
		os.Remove(path)
	}
}

const (
	flagAgentName = "agent-name"
	subagentArgv  = "claude --agent-name impl-x"
	renamedPeer   = "Renamed"
	peerTestUUID  = "11111111-2222-4333-8444-555555555555"
	subagentName  = "impl-x"
	peerHandle    = "NeatDace"
)

func TestArgvFlag(t *testing.T) {
	const argv = "/path/claude --agent-id impl-x@session-9 --agent-name impl-x --team-name session-9 --effort medium"
	tests := []struct {
		name string
		argv string
		flag string
		want string
	}{
		{"separate words", argv, flagAgentName, subagentName},
		{"agent id", argv, "agent-id", "impl-x@session-9"},
		{"equals form", "/path/claude --agent-name=impl-y", flagAgentName, "impl-y"},
		{"absent", "/path/claude --permission-mode auto", flagAgentName, ""},
		{"trailing flag with no value", "/path/claude --agent-name", flagAgentName, ""},
		{"empty argv", "", flagAgentName, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := argvFlag(tc.argv, tc.flag); got != tc.want {
				t.Fatalf("argvFlag(%q) = %q, want %q", tc.flag, got, tc.want)
			}
		})
	}
}

func TestPeerFromEnvNilWithoutSocket(t *testing.T) {
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	if p := PeerFromEnv(context.Background(), &Agent{UUID: "u", Handle: "H"}, ""); p != nil {
		t.Fatalf("no messaging socket must yield no peer; got %+v", p)
	}
}

func TestPeerFromEnvNameSources(t *testing.T) {
	// A subagent's peer name is readable from its own argv; a top-level
	// session's is not, and must record empty rather than guess.
	tests := []struct {
		name     string
		argv     string
		envName  string
		override string
		want     string
	}{
		{"subagent argv", subagentArgv, "", "", subagentName},
		{"top-level session", "claude --permission-mode auto", "", "", ""},
		{"env override", subagentArgv, renamedPeer, "", renamedPeer},
		{"flag beats env and argv", subagentArgv, renamedPeer, "Explicit", "Explicit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/tmp/cc-socks/4242.sock")
			t.Setenv("CLAUDE_PID", "4242")
			t.Setenv("LOTO_PEER_NAME", tc.envName)
			t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-1")
			prev := procArgv
			procArgv = func(context.Context, int) string { return tc.argv }
			defer func() { procArgv = prev }()

			p := PeerFromEnv(context.Background(), &Agent{UUID: "u", Handle: "H"}, tc.override)
			if p == nil {
				t.Fatal("want a peer record")
			}
			if p.Name != tc.want {
				t.Fatalf("Name = %q, want %q", p.Name, tc.want)
			}
			if p.PID != 4242 || p.SessionID != "sess-1" || p.Handle != "H" {
				t.Fatalf("peer lost env context: %+v", p)
			}
			if p.Named() != (tc.want != "") {
				t.Fatalf("Named() = %v for name %q", p.Named(), p.Name)
			}
		})
	}
}

func TestEnvPIDFallsBackToSocketBasename(t *testing.T) {
	t.Setenv("CLAUDE_PID", "")
	if got := envPID("/tmp/cc-socks/777.sock"); got != 777 {
		t.Fatalf("envPID = %d, want 777 from the socket basename", got)
	}
	if got := envPID("/tmp/cc-socks/not-a-pid.sock"); got != 0 {
		t.Fatalf("envPID = %d, want 0 when nothing resolves", got)
	}
}

func TestRecordPeerRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sockDir := t.TempDir()
	sock, kill := liveSocket(t, sockDir)
	defer kill()

	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", sock)
	// A real, running pid: the liveness oracle probes the recorded pid, so a
	// fabricated one would read as a dead session.
	t.Setenv("CLAUDE_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LOTO_PEER_NAME", "")
	prev := procArgv
	procArgv = func(context.Context, int) string { return subagentArgv }
	defer func() { procArgv = prev }()

	a := &Agent{UUID: peerTestUUID, Handle: peerHandle}
	p, err := RecordPeer(context.Background(), a, "")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.Name != subagentName {
		t.Fatalf("RecordPeer = %+v, want name impl-x", p)
	}

	peers, err := Peers(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Handle != peerHandle || peers[0].Name != subagentName {
		t.Fatalf("Peers = %+v, want one live NeatDace/impl-x row", peers)
	}
	if !peers[0].Live(context.Background()) {
		t.Fatal("peer with an existing socket must read live")
	}
}

func TestPeersPrunesDeadSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sockDir := t.TempDir()
	sock, kill := liveSocket(t, sockDir)

	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", sock)
	// Real pid, so the socket's disappearance is what kills this peer.
	t.Setenv("CLAUDE_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LOTO_PEER_NAME", "")
	prev := procArgv
	procArgv = func(context.Context, int) string { return subagentArgv }
	defer func() { procArgv = prev }()

	a := &Agent{UUID: peerTestUUID, Handle: peerHandle}
	if _, err := RecordPeer(context.Background(), a, ""); err != nil {
		t.Fatal(err)
	}
	kill() // the session exits: its socket goes away

	peers, err := Peers(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Live(context.Background()) {
		t.Fatalf("--all must still surface the dead row: %+v", peers)
	}
	// Listing a dead peer unlinks it — presence prunes itself rather than
	// leaning on the lock-liveness check (loto-r11w).
	record := filepath.Join(home, ".loto", "peers", a.UUID+".json")
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("dead peer record must be pruned; stat err = %v", err)
	}
	if peers, err = Peers(context.Background(), false); err != nil || len(peers) != 0 {
		t.Fatalf("Peers(false) = %+v, %v; want empty", peers, err)
	}
}

func TestPeersEmptyWithoutRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	peers, err := Peers(context.Background(), false)
	if err != nil {
		t.Fatalf("a missing peers dir is not an error: %v", err)
	}
	if len(peers) != 0 {
		t.Fatalf("want no peers, got %+v", peers)
	}
}

func TestPeersSortNewestFirst(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sockDir := t.TempDir()
	sock, kill := liveSocket(t, sockDir)
	defer kill()

	now := time.Now().UTC()
	older := &Peer{UUID: peerTestUUID, Handle: "Older", Socket: sock, SeenAt: now.Add(-time.Hour)}
	newer := &Peer{UUID: "22222222-2222-4333-8444-555555555555", Handle: "Newer", Socket: sock, SeenAt: now}
	for _, p := range []*Peer{older, newer} {
		if err := writePeer(p); err != nil {
			t.Fatal(err)
		}
	}
	peers, err := Peers(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 || peers[0].Handle != "Newer" {
		t.Fatalf("want newest-seen first, got %+v", peers)
	}
}

func TestPeerPathRejectsTraversal(t *testing.T) {
	if _, err := peerPath("../../etc/passwd"); err == nil {
		t.Fatal("peerPath must reject a non-uuid id")
	}
}
