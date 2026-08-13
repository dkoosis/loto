package identity

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// peerRaceHome sets up a HOME with a peers dir and a live socket, and stubs
// procArgv, so a test can write records whose liveness is deterministic:
// Socket = the returned sock reads live, a Socket pointing at a nonexistent
// path reads dead ("socket-missing") with no `ps` involved.
func peerRaceHome(t *testing.T) (home, sock string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	// Unix socket paths are length-capped (~104 bytes on darwin) and t.TempDir()
	// embeds the test name — too long for these. Keep the socket dir short.
	//nolint:usetesting // t.TempDir() embeds the test name, and these names overrun darwin's ~104-byte sockaddr_un limit
	sockDir, err := os.MkdirTemp("", "lt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock, kill := liveSocket(t, sockDir)
	t.Cleanup(kill)
	t.Setenv("CLAUDE_PID", strconv.Itoa(os.Getpid()))
	prev := procArgv
	procArgv = func(context.Context, int) string { return subagentArgv }
	t.Cleanup(func() { procArgv = prev })
	return home, sock
}

// deadPeer builds a record whose socket path does not exist, so
// SessionVerdict returns dead/socket-missing without probing the proc table.
func deadPeer(t *testing.T, sockName string) *Peer {
	t.Helper()
	return &Peer{
		UUID:   peerTestUUID,
		Handle: peerHandle,
		Socket: filepath.Join(t.TempDir(), sockName),
		PID:    os.Getpid(),
		SeenAt: time.Now().UTC(),
	}
}

// TestPeersDoesNotUnlinkRecordRewrittenDuringVerdict is the loto-gj1z
// acceptance test: a hook republishes the peer record between readPeerFile's
// read and its unlink — simulating a session restarting inside the seconds-wide
// SessionVerdict window — and Peers must NOT delete the fresh record. It must
// also report it, on this same invocation.
func TestPeersDoesNotUnlinkRecordRewrittenDuringVerdict(t *testing.T) {
	home, sock := peerRaceHome(t)
	if err := writePeer(deadPeer(t, "gone.sock")); err != nil {
		t.Fatal(err)
	}

	fired := 0
	peerPruneRecheckHook = func() {
		if fired > 0 {
			return // one-shot: the restarted session publishes once
		}
		fired++
		live := &Peer{UUID: peerTestUUID, Handle: peerHandle, Socket: sock, PID: os.Getpid(), SeenAt: time.Now().UTC()}
		if werr := writePeer(live); werr != nil {
			t.Errorf("hook republish: %v", werr)
		}
	}
	t.Cleanup(func() { peerPruneRecheckHook = nil })

	peers, err := Peers(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("hook fired %d times, want 1 — the read→unlink window was never entered", fired)
	}
	record := filepath.Join(home, ".loto", "peers", peerTestUUID+".json")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("the republished record was unlinked by a verdict it never earned: %v", err)
	}
	p, ok := readPeerRaw(record)
	if !ok || p.Socket != sock {
		t.Fatalf("surviving record is not the fresh one: ok=%v socket=%q want %q", ok, p.Socket, sock)
	}
	if len(peers) != 1 || peers[0].Socket != sock {
		t.Fatalf("Peers must report the fresh peer on this same invocation: %+v", peers)
	}
}

// TestReadPeerFileDeclinesAfterRepeatedReplacement pins the bounded-retry
// contract: a record replaced on every attempt is re-judged exactly
// peerReadAttempts times, then left alone — no spin, and nothing unlinked that
// was not verified.
func TestReadPeerFileDeclinesAfterRepeatedReplacement(t *testing.T) {
	home, _ := peerRaceHome(t)
	if err := writePeer(deadPeer(t, "gone.sock")); err != nil {
		t.Fatal(err)
	}

	calls := 0
	peerPruneRecheckHook = func() {
		calls++
		// Another DEAD record, at a different (still nonexistent) socket: the
		// replacement is real, so the dev+ino guard must decline every time.
		if werr := writePeer(deadPeer(t, "gone"+strconv.Itoa(calls)+".sock")); werr != nil {
			t.Errorf("hook republish: %v", werr)
		}
	}
	t.Cleanup(func() { peerPruneRecheckHook = nil })

	peers, err := Peers(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if calls != peerReadAttempts {
		t.Fatalf("hook fired %d times, want exactly peerReadAttempts=%d", calls, peerReadAttempts)
	}
	record := filepath.Join(home, ".loto", "peers", peerTestUUID+".json")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("a record replaced on every attempt must be left alone, not unlinked: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("--all must still surface the observation: %+v", peers)
	}
}

// TestPeersSweepsAgedTmpOrphansOnly asserts the crash-debris sweep: a *.tmp
// older than tmpOrphanGrace goes, a fresh one — which may be a live writer's
// in-flight publish — never does.
func TestPeersSweepsAgedTmpOrphansOnly(t *testing.T) {
	home, sock := peerRaceHome(t)
	live := &Peer{UUID: peerTestUUID, Handle: peerHandle, Socket: sock, PID: os.Getpid(), SeenAt: time.Now().UTC()}
	if err := writePeer(live); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(home, ".loto", "peers")
	aged := filepath.Join(dir, peerTestUUID+".aged.tmp")
	fresh := filepath.Join(dir, peerTestUUID+".fresh.tmp")
	for _, p := range []string{aged, fresh} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * tmpOrphanGrace)
	if err := os.Chtimes(aged, old, old); err != nil {
		t.Fatal(err)
	}

	peers, err := Peers(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(aged); !os.IsNotExist(err) {
		t.Fatalf("aged .tmp crash debris must be swept; stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a fresh .tmp may be a live writer's in-flight publish and must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, peerTestUUID+".json")); err != nil {
		t.Fatalf("the live record must be untouched: %v", err)
	}
	if len(peers) != 1 || peers[0].Handle != peerHandle {
		t.Fatalf("no .tmp entry may reach output: %+v", peers)
	}
}

// TestWritePeerCleansTmpOnPublishFailure proves writePeer's deferred cleanup
// covers the reachable error paths, so only a hard kill can leak a .tmp — which
// is what makes the sweep's one-hour grace safe.
func TestWritePeerCleansTmpOnPublishFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".loto", "peers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory where the record belongs: os.Rename cannot publish over it.
	if err := os.Mkdir(filepath.Join(dir, peerTestUUID+".json"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := writePeer(&Peer{UUID: peerTestUUID, Handle: peerHandle, SeenAt: time.Now().UTC()})
	if err == nil {
		t.Fatal("writePeer must fail when the record path is not writable")
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("failed publish leaked a temp: %s", e.Name())
		}
	}
}

// TestPeerJSONWithoutProcStartUnmarshalsToZero is the entire compatibility
// story for the ProcStart field, made a visible test rather than an assumed
// property: a record written by the pre-uxhg binary — no proc_start key at
// all — must unmarshal with ProcStart == 0 (unknown) and every other field
// intact.
func TestPeerJSONWithoutProcStartUnmarshalsToZero(t *testing.T) {
	const body = `{
		"uuid": "` + peerTestUUID + `",
		"handle": "` + peerHandle + `",
		"socket": "/tmp/cc-socks/4242.sock",
		"pid": 4242,
		"session_id": "sess-1",
		"seen_at": "2026-08-13T00:00:00Z"
	}`
	var p Peer
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("unmarshal pre-uxhg record shape: %v", err)
	}
	if p.ProcStart != 0 {
		t.Fatalf("ProcStart = %d, want 0 (unknown) for a record with no proc_start key", p.ProcStart)
	}
	if p.UUID != peerTestUUID || p.Handle != peerHandle || p.PID != 4242 || p.SessionID != "sess-1" {
		t.Fatalf("other fields must still parse: %+v", p)
	}
}
