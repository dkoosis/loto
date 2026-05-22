package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestE2E_TagLifecycle walks the full §Tasks T8 scenario: lock → tag → status
// echoes tag → ack → status drops it → unlock implicit-acks → re-acquire shows
// previous tags dead → doctor --repair runs clean.
func TestE2E_TagLifecycle(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	asAlice := func() { t.Setenv("LOTO_AGENT_ID", alice.UUID) }
	asBob := func() { t.Setenv("LOTO_AGENT_ID", bob.UUID) }
	run := func(argv ...string) (string, string, int) {
		var out, errBuf bytes.Buffer
		code := Run(argv, &out, &errBuf)
		return out.String(), errBuf.String(), code
	}

	// 1. alice locks a.go.
	asAlice()
	if _, _, code := run(tcCmdLock, tcTargetA, "-t", tcIntentTest); code != 0 {
		t.Fatalf("alice lock: code=%d", code)
	}

	// 2. bob tries to lock — conflict, clean (no tags yet).
	asBob()
	out2, err2, code := run(tcCmdLock, tcTargetA, "-t", tcIntentTest)
	if code != 1 {
		t.Fatalf("bob lock should conflict, code=%d", code)
	}
	if strings.Contains(out2+err2, "ℹ   tag id=") {
		t.Fatalf("conflict output should be clean of tag rows: %q / %q", out2, err2)
	}

	// 3. bob tags a.go.
	bobTagOut, _, code := run("tag", tcTargetA, "ETA?")
	if code != 0 {
		t.Fatalf("bob tag: code=%d out=%q", code, bobTagOut)
	}
	bobTagID := parseTagIDFromOutput(t, bobTagOut)

	// 4. alice status a.go — sees ETA? via inline + trailing footer.
	asAlice()
	out4, _, code := run(tcCmdStatus, tcTargetA)
	if code != 0 {
		t.Fatalf("alice status: code=%d", code)
	}
	if !strings.Contains(out4, "ETA?") {
		t.Fatalf("alice status should show bob's tag: %q", out4)
	}

	// 5. alice self-tags (edge #2).
	if _, _, code := run("tag", tcTargetA, "self note"); code != 0 {
		t.Fatalf("alice self-tag: code=%d", code)
	}

	// 6. alice acks bob's tag.
	if _, _, code := run("ack", bobTagID); code != 0 {
		t.Fatalf("alice ack: code=%d", code)
	}

	// 7. alice status — bob's tag is gone, self note remains (visible via
	//    inline rows from ListAliveForTarget which has no self-filter).
	out7, _, code := run(tcCmdStatus, tcTargetA)
	if code != 0 {
		t.Fatalf("alice status post-ack: code=%d", code)
	}
	if strings.Contains(out7, "ETA?") {
		t.Fatalf("bob's tag should be dismissed: %q", out7)
	}
	if !strings.Contains(out7, "self note") {
		t.Fatalf("self note should still appear via target inline: %q", out7)
	}

	// 8. alice unlocks — should implicit-ack the remaining self note.
	if _, _, code := run(tcCmdUnlock, tcTargetA, "-t", tcIntentDone); code != 0 {
		t.Fatalf("alice unlock: code=%d", code)
	}

	// 9. alice re-acquires — different lock_created_at, previous tags stay dead.
	if _, _, code := run(tcCmdLock, tcTargetA, "-t", tcIntentTest); code != 0 {
		t.Fatalf("alice re-lock: code=%d", code)
	}
	out9, _, code := run(tcCmdStatus, tcTargetA)
	if code != 0 {
		t.Fatalf("alice post-relock status: code=%d", code)
	}
	if strings.Contains(out9, "self note") || strings.Contains(out9, "ETA?") {
		t.Fatalf("previous-lock tags should be dead after re-acquire: %q", out9)
	}

	// 10. doctor --repair runs clean.
	if _, _, code := run(tcCmdDoctor, tcFlagRepair); code != 0 {
		t.Fatalf("alice doctor --repair: code=%d", code)
	}
}

func parseTagIDFromOutput(t *testing.T, s string) string {
	t.Helper()
	const marker = "id="
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("no id= in %q", s)
	}
	rest := s[i+len(marker):]
	j := strings.Index(rest, " ")
	if j < 0 {
		t.Fatalf("malformed: %q", s)
	}
	return rest[:j]
}
