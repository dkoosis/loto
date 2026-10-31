package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// asActor puts this test in the env of one Claude Code session: session id
// sid in the process pid.
func asActor(t *testing.T, sid string, pid int) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
	t.Setenv("LOTO_PID", strconv.Itoa(pid))
}

func actorEditEvent(repo, toolUseID, agentID string) string {
	return fmt.Sprintf(
		`{"session_id":"s","tool_name":"Edit","tool_use_id":%q,"cwd":%q,"agent_id":%q,"tool_input":{"file_path":%q}}`,
		toolUseID, repo, agentID, filepath.Join(repo, tcTargetA))
}

// The bead's first acceptance criterion (loto-e1ku): two session ids on one
// pid are one actor, a subagent's write over its parent's hold is admitted,
// and another process's is refused. Both live pids are real (this test binary
// and its parent), so the hold is live for the peer.
func TestWriteGateActorIsTheProcess(t *testing.T) {
	repo := withTempProject(t)
	asActor(t, "sess-parent", os.Getpid())
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", "parent's hold"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("parent lock: exit %d", code)
	}

	// The same process after /clear, writing through one of its subagents.
	asActor(t, "sess-after-clear", os.Getpid())
	if _, stderr, code := runHookEvent(t, "pre", actorEditEvent(repo, "tu-sub", "agent-sub")); code != 0 {
		t.Fatalf("subagent write over its parent's hold: exit %d, want admitted; stderr:\n%s", code, stderr)
	}

	asActor(t, "sess-peer", os.Getppid())
	_, stderr, code := runHookEvent(t, "pre", actorEditEvent(repo, "tu-peer", ""))
	if code != 2 {
		t.Fatalf("another process's write: exit %d, want 2 (refused); stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, tcTargetA) {
		t.Fatalf("refusal does not name %s:\n%s", tcTargetA, stderr)
	}
}

// The bead's second acceptance criterion: unlock --all releases every hold
// of the actor, in every worktree, under every session id it has had.
func TestUnlockAllReleasesActorInEveryWorktree(t *testing.T) {
	repo := withTempProject(t)
	asActor(t, "sess-1", os.Getpid())
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", "main"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock in main checkout: exit %d", code)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	mustGit(t, repo, "worktree", "add", "-q", "-b", "side", wt)
	if err := os.WriteFile(filepath.Join(wt, tcTargetA), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wt)
	asActor(t, "sess-2", os.Getpid())
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", "linked"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock in linked worktree: exit %d", code)
	}
	if code := Run([]string{"claim", "pkg", "-t", "linked claim"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("claim in linked worktree: exit %d", code)
	}

	t.Chdir(repo)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdUnlock, "--all"}, &out, &errBuf); code != 0 {
		t.Fatalf("unlock --all: exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errBuf.String())
	}
	rt, done := hookStoreRead(t)
	defer done()
	left, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("unlock --all left %d holds, want 0: %+v", len(left), left)
	}
	claims, err := rt.Store.ListClaims(rt.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("unlock --all left %d claims, want 0: %+v", len(claims), claims)
	}
}
