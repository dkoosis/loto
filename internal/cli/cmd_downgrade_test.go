package cli

import (
	"bytes"
	"testing"
)

func TestCmdDowngrade_ExclusiveToShared(t *testing.T) {
	withTempProject(t)
	pinAgent(t) // alice
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", "write"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice exclusive lock failed, exit %d", code)
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{"downgrade", tcTargetA}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("downgrade should succeed; code=%d out=%q err=%q", code, out.String(), errBuf.String())
	}

	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t) // bob
	if c2 := Run([]string{tcCmdLock, tcTargetA, "-t", "read", "--shared"}, &bytes.Buffer{}, &bytes.Buffer{}); c2 != 0 {
		t.Fatalf("shared lock should succeed after downgrade; code=%d", c2)
	}
}

func TestCmdDowngrade_NoLock(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{"downgrade", tcTargetA}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("downgrade with no lock held should exit 1; code=%d out=%q", code, out.String())
	}
}
