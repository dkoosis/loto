package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	cmdWhoName = "who"
	// flagJSON is shared with the whoami tests so goconst does not flag the
	// repeated literal.
	flagJSON = "--json"
)

// recordPeer plants a peer record for handle under a temp HOME, backed by a
// real unix socket (the liveness witness `who` reads). Returns the socket's
// kill func so a test can simulate the session exiting.
func recordPeer(t *testing.T, handle, peerName string) func() {
	t.Helper()
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "s.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", sockPath)
	t.Setenv("CLAUDE_PID", "4242")
	t.Setenv("LOTO_PEER_NAME", peerName)
	t.Setenv("LOTO_HANDLE", handle)
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	var out, errBuf bytes.Buffer
	if code := Run([]string{cmdWhoamiName}, &out, &errBuf); code != 0 {
		t.Fatalf("whoami exit=%d err=%q", code, errBuf.String())
	}
	return func() {
		l.Close()
		os.Remove(sockPath)
	}
}

func TestWhoListsRecordedPeer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer recordPeer(t, "NeatDace", "itzy-8c")()

	var out bytes.Buffer
	if code := Run([]string{cmdWhoName}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := out.String()
	for _, want := range []string{"peers: 1", "handle=NeatDace", "peer=itzy-8c", "state=live"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in who output: %q", want, got)
		}
	}
}

func TestWhoFlagsUnnamedPeer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// A top-level session: Claude Code names it from cwd or /rename, and that
	// name is unreadable from a hook — the row must say so rather than guess.
	defer recordPeer(t, "GoldBadger", "")()

	var out bytes.Buffer
	if code := Run([]string{cmdWhoName}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := out.String()
	if !strings.Contains(got, "peer=-") {
		t.Fatalf("unnamed peer must render as peer=-: %q", got)
	}
	if !strings.Contains(got, "/rename") {
		t.Fatalf("unnamed peer must carry the remedy: %q", got)
	}
}

func TestWhoByHandle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer recordPeer(t, "NeatDace", "itzy-8c")()

	var out, errBuf bytes.Buffer
	// Case-insensitive, like every other handle lookup in loto.
	if code := Run([]string{cmdWhoName, "neatdace"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit=%d err=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "peer=itzy-8c") {
		t.Fatalf("want the peer name for the asked handle: %q", out.String())
	}

	out.Reset()
	errBuf.Reset()
	// A handle with no live peer is a "no" answer, not a crash: exit 1.
	if code := Run([]string{cmdWhoName, "NoSuchAgent"}, &out, &errBuf); code != 1 {
		t.Fatalf("unknown handle exit=%d, want 1 (err=%q)", code, errBuf.String())
	}
}

func TestWhoJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer recordPeer(t, "NeatDace", "itzy-8c")()

	var out bytes.Buffer
	if code := Run([]string{cmdWhoName, flagJSON}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got []struct {
		Handle string `json:"handle"`
		Name   string `json:"name"`
		Socket string `json:"socket"`
		PID    int    `json:"pid"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not a JSON array: %v\noutput: %q", err, out.String())
	}
	if len(got) != 1 || got[0].Handle != "NeatDace" || got[0].Name != "itzy-8c" || got[0].PID != 4242 {
		t.Fatalf("unexpected rows: %+v", got)
	}
	if got[0].Socket == "" {
		t.Fatal("socket path is the liveness witness; it must be in the JSON")
	}
}

func TestWhoEmptyWithoutPeers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")

	var out bytes.Buffer
	if code := Run([]string{cmdWhoName}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(out.String(), "peers: 0") {
		t.Fatalf("want an empty table, got %q", out.String())
	}
}

func TestWhoRejectsExtraArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var errBuf bytes.Buffer
	if code := Run([]string{cmdWhoName, "A", "B"}, &bytes.Buffer{}, &errBuf); code != 2 {
		t.Fatalf("exit=%d, want 2 on usage error", code)
	}
}

func TestWhoamiRecordsAndReportsPeer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer recordPeer(t, "NeatDace", "itzy-8c")()

	var out bytes.Buffer
	if code := Run([]string{cmdWhoamiName, flagJSON}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		Peer string `json:"peer"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out.String())
	}
	if got.Peer != "itzy-8c" {
		t.Fatalf("whoami --json peer = %q, want itzy-8c", got.Peer)
	}
}

func TestWhoamiPeerNameFlagOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer recordPeer(t, "NeatDace", "")()

	var out bytes.Buffer
	if code := Run([]string{cmdWhoamiName, "--peer-name", "atc"}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "peer:   atc") {
		t.Fatalf("--peer-name must be recorded and echoed: %q", out.String())
	}

	out.Reset()
	if code := Run([]string{cmdWhoName}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "peer=atc") {
		t.Fatalf("who must show the recorded name: %q", out.String())
	}
}
