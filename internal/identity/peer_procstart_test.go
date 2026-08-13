//go:build linux || darwin

package identity

import (
	"context"
	"os"
	"strconv"
	"testing"
)

// TestPeerFromEnvRecordsProcStart proves the recording half of D2: PeerFromEnv
// reads this process's real start-time and the value survives a JSON
// round-trip through writePeer/readPeerRaw. Tagged linux || darwin since
// procstart_other.go always answers unknown, which this test would otherwise
// misreport as a recording bug.
func TestPeerFromEnvRecordsProcStart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/tmp/cc-socks/4242.sock")
	t.Setenv("CLAUDE_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LOTO_PEER_NAME", "")
	prev := procArgv
	procArgv = func(context.Context, int) string { return topLevelArgv }
	t.Cleanup(func() { procArgv = prev })

	a := &Agent{UUID: peerTestUUID, Handle: peerHandle}
	p := PeerFromEnv(context.Background(), a, "")
	if p == nil {
		t.Fatal("want a peer record")
	}
	want, ok := ProcStart(os.Getpid())
	if !ok {
		t.Fatal("ProcStart(self) must be readable on this platform")
	}
	if p.ProcStart == 0 || p.ProcStart != want {
		t.Fatalf("PeerFromEnv ProcStart = %d, want %d (a direct ProcStart(self) read)", p.ProcStart, want)
	}

	if err := writePeer(p); err != nil {
		t.Fatal(err)
	}
	path, err := peerPath(p.UUID)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := readPeerRaw(path)
	if !ok || got.ProcStart != p.ProcStart {
		t.Fatalf("round-tripped ProcStart = %d ok=%v, want %d", got.ProcStart, ok, p.ProcStart)
	}
}
