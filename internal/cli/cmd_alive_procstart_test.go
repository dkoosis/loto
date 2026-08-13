//go:build linux || darwin

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/identity"
)

// TestAliveDeadOnRecycledPID is the CLI surface proof for AC-4: a flagless
// top-level peer whose recorded start-time disagrees with the pid's current
// one must reach `loto alive` stdout as reason=procstart-mismatch. Tagged
// linux || darwin — see liveness_procstart_test.go for why.
func TestAliveDeadOnRecycledPID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sock := filepath.Join(home, "ghost.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatalf("write socket stand-in: %v", err)
	}
	plantPeer(t, identity.Peer{
		UUID:      aliveTestUUID,
		Handle:    "GhostDace",
		Socket:    sock,
		PID:       os.Getpid(),
		ProcStart: 1, // this process's real start-time can never be 1
		SeenAt:    time.Now().UTC(),
	})

	out, code := runAlive(t, "ghostdace")
	if code != 1 {
		t.Fatalf("exit=%d, want 1 when a queried subject is dead\n%s", code, out)
	}
	for _, want := range []string{
		"dead=1",
		"✗ agent=GhostDace uuid=" + aliveTestUUID,
		"reason=procstart-mismatch",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
