//go:build linux || darwin

package identity

import (
	"context"
	"os"
	"testing"
)

// TestSessionVerdictDeadOnRecycledPIDForFlaglessPeer is the bead's AC-1
// acceptance test: a flagless top-level peer (empty AgentID, empty
// ParentSessionID — the shape neither --agent-id nor --parent-session-id
// ever takes) whose recorded start-time disagrees with the pid's current
// start-time must read DEAD. This is the defect loto-uxhg closes.
//
// The real reader is used deliberately, uninstrumented: this process's true
// start-time can never equal the fabricated ProcStart of 1, on either
// encoding (darwin wall-clock µs, linux ticks-since-boot). Tagged
// linux || darwin because procstart_other.go always answers unknown, which
// would make this test pass for the wrong reason (falling through to argv).
func TestSessionVerdictDeadOnRecycledPIDForFlaglessPeer(t *testing.T) {
	sock := existingSocket(t)
	stubArgv(t, topLevelArgv) // no real ps runs; the start-time witness must fire first

	p := &Peer{
		UUID:            oracleUUID,
		Handle:          oracleHandle,
		Socket:          sock,
		PID:             os.Getpid(),
		AgentID:         "",
		ParentSessionID: "",
		ProcStart:       1, // this process's real start-time can never be 1
	}
	got := p.SessionVerdict(context.Background())
	if got.Liveness != SessionDead || got.Reason != reasonProcStartMismatch {
		t.Fatalf("SessionVerdict = %s/%s, want dead/%s", got.Liveness, got.Reason, reasonProcStartMismatch)
	}

	// Pre-fix equivalent, recorded so this is provably a bug test that was once
	// red: the same peer minus ProcStart takes the legacy path and reads LIVE —
	// this is main@143d323's answer for the exact scenario the bead reports.
	legacy := *p
	legacy.ProcStart = 0
	if got := legacy.SessionVerdict(context.Background()); got.Liveness != SessionLive || got.Reason != reasonPSMatch {
		t.Fatalf("pre-fix equivalent (ProcStart=0) = %s/%s, want live/%s — the bug this bead closes",
			got.Liveness, got.Reason, reasonPSMatch)
	}
}
