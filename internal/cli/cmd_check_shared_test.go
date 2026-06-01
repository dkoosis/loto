package cli

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestCheck_SharedPeerNotConflict: a peer holding a shared lock does not block
// a committer's check (shared+shared coexist). Exit 0 (loto-k5el.2 T8).
func TestCheck_SharedPeerNotConflict(t *testing.T) {
	withTempProject(t)
	pinAgent(t) // alice
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentRead, tcFlagShared}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice shared lock failed, exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t) // bob
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdCheck, tcTargetA}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("shared peer must not block check; code=%d out=%q", code, out.String())
	}
}

// TestCheck_AliveExclusivePeerBlocks: a provably-live exclusive holder is a hard
// blocker — exit 1 (binding correction 4). LOTO_PID = this live test process so
// the holder classifies ALIVE.
func TestCheck_AliveExclusivePeerBlocks(t *testing.T) {
	withTempProject(t)
	pinAgent(t)                                     // alice
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid())) // durable, alive
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentWrite}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice exclusive lock failed, exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t) // bob
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdCheck, tcTargetA}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("alive exclusive peer must hard-block; code=%d out=%q", code, out.String())
	}
}

// TestCheck_UnknownExclusivePeerWarns: an exclusive holder with no durable
// liveness handle (PID-0 sentinel) is indeterminate — warn, do not hard-block
// (exit 0 with ⚠). binding correction 4 / §check --staged.
func TestCheck_UnknownExclusivePeerWarns(t *testing.T) {
	withTempProject(t)
	pinAgent(t)              // alice
	t.Setenv("LOTO_PID", "") // PID-0 sentinel → liveness UNKNOWN
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentWrite}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice exclusive lock failed, exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t) // bob
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdCheck, tcTargetA}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("unknown-liveness exclusive peer must warn, not block; code=%d out=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "liveness=unknown") || !strings.Contains(out.String(), "blocking=0") {
		t.Fatalf("expected advisory liveness=unknown + blocking=0 row; got %q", out.String())
	}
}
