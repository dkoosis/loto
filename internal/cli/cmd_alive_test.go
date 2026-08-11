package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/identity"
)

const (
	cmdAliveName = "alive"
	// aliveTestUUID is canonical-shape hex so peerPath accepts it.
	aliveTestUUID = "11111111-2222-3333-4444-555555555555"
)

// plantPeer writes a peer record straight into the temp HOME's registry, which
// is how a test fabricates a session it does not own: socket path and pid are
// the only evidence the oracle reads, so both are chosen by the caller.
func plantPeer(t *testing.T, p identity.Peer) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".loto", "peers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir peers: %v", err)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal peer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, p.UUID+".json"), body, 0o600); err != nil {
		t.Fatalf("write peer: %v", err)
	}
}

func runAlive(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := Run(append([]string{cmdAliveName}, args...), &out, &errBuf)
	if errBuf.Len() > 0 {
		t.Logf("stderr: %s", errBuf.String())
	}
	return out.String(), code
}

// TestAliveUnknownHandleIsAdvisory: no peer record is an absence of evidence,
// so it must not fail the command — a caller that exited non-zero here would
// treat "I never heard of it" as "it is dead".
func TestAliveUnknownHandleIsAdvisory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	out, code := runAlive(t, "NoSuchAgent")
	if code != 0 {
		t.Fatalf("exit=%d, want 0 for an unknown subject", code)
	}
	for _, want := range []string{
		"ℹ checked=1 live=0 dead=0 unknown=1",
		"⚠ agent=NoSuchAgent uuid=- liveness=unknown reason=no-peer-record",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// TestAliveDeadPeerFails: the socket is the witness a live session keeps on
// disk, so a recorded socket that is gone is the one provable death.
func TestAliveDeadPeerFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plantPeer(t, identity.Peer{
		UUID:   aliveTestUUID,
		Handle: "GoneBadger",
		Socket: filepath.Join(home, "no-such.sock"),
		PID:    os.Getpid(),
		SeenAt: time.Now().UTC(),
	})

	out, code := runAlive(t, "GoneBadger")
	if code != 1 {
		t.Fatalf("exit=%d, want 1 when a queried subject is dead\n%s", code, out)
	}
	want := "✗ agent=GoneBadger uuid=" + aliveTestUUID + " liveness=dead reason=socket-missing"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in:\n%s", want, out)
	}
	if !strings.Contains(out, "dead=1") {
		t.Fatalf("triage counts must report the death:\n%s", out)
	}
}

// TestAliveLivePeerPasses drives the pass side through the same fabricated
// record: os.Stat is the socket check, so a plain file is a sufficient witness.
func TestAliveLivePeerPasses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sock := filepath.Join(home, "live.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatalf("write socket stand-in: %v", err)
	}
	plantPeer(t, identity.Peer{
		UUID:   aliveTestUUID,
		Handle: "LivelyDace",
		Socket: sock,
		PID:    os.Getpid(),
		SeenAt: time.Now().UTC(),
	})

	out, code := runAlive(t, "livelydace") // handles match case-insensitively
	if code != 0 {
		t.Fatalf("exit=%d, want 0 for a live subject\n%s", code, out)
	}
	if !strings.Contains(out, "live=1") || !strings.Contains(out, "liveness=live") {
		t.Fatalf("want a live verdict:\n%s", out)
	}
	if !strings.Contains(out, "✓ agent=LivelyDace uuid="+aliveTestUUID) {
		t.Fatalf("row must carry the recorded identity:\n%s", out)
	}
}

// TestAliveEmptyRegistryHeader: an empty answer still prints its header —
// silence looks like a crash (.claude/rules/design.md).
func TestAliveEmptyRegistryHeader(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	out, code := runAlive(t)
	if code != 0 {
		t.Fatalf("exit=%d, want 0 on an empty registry", code)
	}
	if !strings.HasPrefix(out, "ℹ checked=0") {
		t.Fatalf("want an explicit empty header, got %q", out)
	}
}

// TestAliveHelpTeachesKillContract pins the sentence a reaper's author must
// read: the exit code alone does not authorize a kill.
func TestAliveHelpTeachesKillContract(t *testing.T) {
	_, stderr, _ := executeCommand(cmdAliveName, "-h")
	for _, want := range []string{
		"usage: loto alive",
		"necessary but never sufficient",
		"idle/TaskStop",
		"LIVE\nvetoes a kill",
		"fall back to TTL, never treat as",
		"loto alive NeatDace",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("alive -h missing %q; got:\n%s", want, stderr)
		}
	}
}
