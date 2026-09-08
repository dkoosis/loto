package cli

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"loto/internal/render"
)

// Golden tests for `loto check --moved` (loto-ea8y.2). The surface is a
// post-checkout advisory, so the two things worth pinning exactly are the
// ⚠ block a mover reads and the ✓ line the hook turns into silence.

const (
	tcFlagMoved = "--moved"
	// tcMovedClean is the pass line the post-checkout entry turns into
	// silence — the one the hook's own golden asserts it drops.
	tcMovedClean = "✓ moved-peer-locks count=0\n"
)

// gitOutT runs git in dir and returns trimmed stdout.
func gitOutT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// twoHeads seeds the repo with two commits that differ in tcTargetA and
// returns their shas.
func twoHeads(t *testing.T, repo string) (oldHead, newHead string) {
	t.Helper()
	writeT(t, repo, tcTargetA, "one")
	gitT(t, repo, "add", tcTargetA)
	gitT(t, repo, "commit", "-q", "-m", "one")
	oldHead = gitOutT(t, repo, "rev-parse", "HEAD")
	writeT(t, repo, tcTargetA, "two")
	gitT(t, repo, "add", tcTargetA)
	gitT(t, repo, "commit", "-q", "-m", "two")
	newHead = gitOutT(t, repo, "rev-parse", "HEAD")
	return oldHead, newHead
}

func movedRun(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := Run(append([]string{tcCmdCheck, tcFlagMoved}, args...), &out, &errBuf)
	t.Logf("stderr: %q", errBuf.String())
	return scrubTime(out.String()), code
}

// AC 1: a peer holds A, A differs between the two HEADs → exit 0 and one ⚠
// row naming A and the holder.
func TestCheckMoved_PeerLockedPathWarnsAndNeverBlocks(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	oldHead, newHead := twoHeads(t, repo)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)

	got, code := movedRun(t, oldHead, newHead, "1")
	if code != 0 {
		t.Fatalf("a tree move is never blocked; got exit %d: %q", code, got)
	}
	want := "⚠ moved-peer-locks count=1\n" +
		"⚠ path=a.go kind=lock blocker=" + alice.UUID + " intent=\"test\" expires_at=<T>\n" +
		"ℹ options=tell-the-holder|move-back|carry-on — the move already happened\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
}

// renamedHeads seeds two commits where tcTargetA became tcTargetC, and
// returns their shas. git detects this as a rename (R100) unless told not to.
func renamedHeads(t *testing.T, repo string) (oldHead, newHead string) {
	t.Helper()
	writeT(t, repo, tcTargetA, "one\ntwo\nthree\n")
	gitT(t, repo, "add", tcTargetA)
	gitT(t, repo, "commit", "-q", "-m", "one")
	oldHead = gitOutT(t, repo, "rev-parse", "HEAD")
	gitT(t, repo, "mv", tcTargetA, tcTargetC)
	gitT(t, repo, "commit", "-q", "-m", "renamed")
	newHead = gitOutT(t, repo, "rev-parse", "HEAD")
	return oldHead, newHead
}

// loto-l9ve AC 1: a peer holds the rename's SOURCE, and the move rewrote it —
// the file was removed from the working tree. `--name-only` alone reports the
// destination only, because diff.renames has defaulted to true since git 2.9;
// the comment claiming that omitting -M disables detection was simply wrong.
// --no-renames is the flag that reports both sides.
func TestCheckMoved_RenameSourceIsNamedWithItsHolder(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	oldHead, newHead := renamedHeads(t, repo)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	// The source is gone from the working tree, so the lock is taken before
	// the rename — which is what a peer editing a.go would have done.
	writeT(t, repo, tcTargetA, "one\ntwo\nthree\n")
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock failed")
	}
	if err := os.Remove(repo + "/" + tcTargetA); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)

	got, code := movedRun(t, oldHead, newHead, "1")
	if code != 0 {
		t.Fatalf("a tree move is never blocked; got exit %d: %q", code, got)
	}
	want := "⚠ moved-peer-locks count=1\n" +
		"⚠ path=a.go kind=lock blocker=" + alice.UUID + " intent=\"test\" expires_at=<T>\n" +
		"ℹ options=tell-the-holder|move-back|carry-on — the move already happened\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
}

// Both sides of the rename are examined, not just the source: a peer holding
// the DESTINATION is named too, which is what the destination-only behavior
// happened to get right and must keep getting right.
func TestCheckMoved_RenameDestinationIsStillNamed(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	oldHead, newHead := renamedHeads(t, repo)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcTargetC, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)

	got, code := movedRun(t, oldHead, newHead, "1")
	if code != 0 {
		t.Fatalf("got exit %d: %q", code, got)
	}
	if !strings.Contains(got, "path=c.go kind=lock blocker="+alice.UUID) {
		t.Errorf("want the rename destination named: %q", got)
	}
}

// Regression on the existing golden: with no rename between the two HEADs the
// output is unchanged by --no-renames.
func TestCheckMoved_NoRenamesOutputIsUnchanged(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	oldHead, newHead := twoHeads(t, repo)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)

	got, code := movedRun(t, oldHead, newHead, "1")
	want := "⚠ moved-peer-locks count=1\n" +
		"⚠ path=a.go kind=lock blocker=" + alice.UUID + " intent=\"test\" expires_at=<T>\n" +
		"ℹ options=tell-the-holder|move-back|carry-on — the move already happened\n"
	if code != 0 || got != want {
		t.Errorf("output\n got: %q (%d)\nwant: %q", got, code, want)
	}
}

// AC 2: no peer locks → the ✓ line the hook drops, exit 0.
func TestCheckMoved_NoPeerLocksIsAPlainPass(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	oldHead, newHead := twoHeads(t, repo)
	got, code := movedRun(t, oldHead, newHead, "1")
	if code != 0 || got != tcMovedClean {
		t.Errorf("clean move: code=%d out=%q", code, got)
	}
}

// My own lock is not a peer's.
func TestCheckMoved_OwnLockIsNotReported(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	oldHead, newHead := twoHeads(t, repo)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock failed")
	}
	got, code := movedRun(t, oldHead, newHead, "1")
	if code != 0 || got != tcMovedClean {
		t.Errorf("own lock must not warn: code=%d out=%q", code, got)
	}
}

// A file checkout passes the same HEAD twice; nothing changed, nothing to say.
func TestCheckMoved_SameHeadBothSides(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	_, newHead := twoHeads(t, repo)
	got, code := movedRun(t, newHead, newHead, "0")
	if code != 0 || got != tcMovedClean {
		t.Errorf("same head: code=%d out=%q", code, got)
	}
}

// An unreadable ref (the null sha git passes on a first checkout, a shallow
// clone) fails OPEN: exit 0, nothing on stdout, the reason on stderr. A
// non-zero here would stop the hook chain and keep 50-beads from running.
func TestCheckMoved_UnreadableRefFailsOpen(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	_, newHead := twoHeads(t, repo)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagMoved, "0000000000000000000000000000000000000000", newHead, "1"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("want fail-open exit 0, got %d", code)
	}
	if out.String() != "" {
		t.Errorf("fail-open must print nothing on stdout: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "moved=fail-open") {
		t.Errorf("fail-open must be loud on stderr: %q", errBuf.String())
	}
}

func TestCheckMoved_WrongArity(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	for _, args := range [][]string{{}, {"a"}, {"a", "b", "c", "d"}} {
		var out, errBuf bytes.Buffer
		if code := Run(append([]string{tcCmdCheck, tcFlagMoved}, args...), &out, &errBuf); code != 2 {
			t.Errorf("args %v: want exit 2, got %d", args, code)
		}
	}
}

func TestCheckMoved_RefusesGateCombination(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdCheck, tcFlagMoved, tcFlagGate, "a", "b"}, &out, &errBuf); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
}

// ── collapseMovedRows: one row per path ───────────────────────────────────

func TestCollapseMovedRows_LockBeatsClaimOnOnePath(t *testing.T) {
	now := time.Now()
	deny := []render.GateDenyRow{
		{Path: tcTargetA, Kind: render.GateKindClaim, HolderUUID: gateFoeUUID, BlockerPath: tcPrefixParent, ExpiresAt: now},
		{Path: tcTargetA, Kind: render.GateKindLock, HolderUUID: gateMyUUID, BlockerPath: tcTargetA, ExpiresAt: now},
	}
	rows := collapseMovedRows(deny)
	if len(rows) != 1 {
		t.Fatalf("want one row per path, got %+v", rows)
	}
	if rows[0].Kind != render.GateKindLock {
		t.Errorf("the lock names who is actually in the file: %+v", rows[0])
	}
}

func TestCollapseMovedRows_LowestHolderWinsWithinAKind(t *testing.T) {
	now := time.Now()
	deny := []render.GateDenyRow{
		{Path: tcTargetA, Kind: render.GateKindLock, HolderUUID: gateFoeUUID, ExpiresAt: now},
		{Path: tcTargetA, Kind: render.GateKindLock, HolderUUID: gateMyUUID, ExpiresAt: now},
	}
	rows := collapseMovedRows(deny)
	if len(rows) != 1 || rows[0].HolderUUID != gateMyUUID {
		t.Fatalf("tie-break must be stable and lowest-first: %+v", rows)
	}
}

func TestCollapseMovedRows_SortedByPath(t *testing.T) {
	now := time.Now()
	deny := []render.GateDenyRow{
		{Path: tcTargetC, Kind: render.GateKindLock, HolderUUID: gateFoeUUID, ExpiresAt: now},
		{Path: tcTargetA, Kind: render.GateKindLock, HolderUUID: gateFoeUUID, ExpiresAt: now},
		{Path: tcTargetB, Kind: render.GateKindLock, HolderUUID: gateFoeUUID, ExpiresAt: now},
	}
	rows := collapseMovedRows(deny)
	if len(rows) != 3 || rows[0].Path != tcTargetA || rows[1].Path != tcTargetB || rows[2].Path != tcTargetC {
		t.Fatalf("rows must sort by path: %+v", rows)
	}
}
