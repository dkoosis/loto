package cli

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAcceptance_GoldenHappyPath(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	steps := []struct {
		args []string
		want string
	}{
		{[]string{"whoami"}, "handle:"},
		{[]string{tcCmdLock, tcTargetA, tcFlagIntent, "smoke"}, "✓ locked count=1"},
		{[]string{tcCmdStatus, tcFlagMine}, tcTargetA},
		{[]string{tcCmdUnlock, tcTargetA, "-t", tcIntentDone}, "✓ unlocked count=1"},
	}
	for _, s := range steps {
		var out bytes.Buffer
		code := Run(s.args, &out, io.Discard)
		if code != 0 {
			t.Fatalf("%v exit %d: %s", s.args, code, out.String())
		}
		if s.want != "" && !strings.Contains(out.String(), s.want) {
			t.Errorf("%v missing %q in: %s", s.args, s.want, out.String())
		}
	}
}

// TestAcceptance_BasicMultiAgentFlow exercises lock/unlock/check across two agents.
func TestAcceptance_BasicMultiAgentFlow(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, tcFlagIntent, "refactor"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &out, io.Discard); code != 1 {
		t.Fatalf("expected conflict, got %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ blocked") {
		t.Errorf("expected ✗ blocked: %q", out.String())
	}
	out.Reset()
	if code := Run([]string{tcCmdCheck, tcStoreStoreGo}, &out, io.Discard); code != 1 {
		t.Fatalf("check expected exit 1, got %d", code)
	}
	if !strings.Contains(out.String(), "blocker=") {
		t.Errorf("check-paths missing blocker: %q", out.String())
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdUnlock, tcStoreStoreGo, "-t", tcIntentDone}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice unlock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("bob lock should succeed after alice unlock")
	}
}

// TestConcurrentLock_SerializedByOpFlock fires two lock invocations in parallel
// from a single process; op-flock must let both succeed against disjoint files.
// Catches lock-DB races introduced by future schema or transaction changes.
func TestConcurrentLock_SerializedByOpFlock(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	exits := make([]int, 2)
	argSets := [][]string{
		{tcCmdLock, tcTargetA, "-t", tcIntentTest},
		{tcCmdLock, tcTargetB, "-t", tcIntentTest},
	}
	for i, args := range argSets {
		wg.Go(func() {
			exits[i] = Run(args, io.Discard, io.Discard)
		})
	}
	wg.Wait()
	if exits[0] != 0 || exits[1] != 0 {
		t.Errorf("exits: %v", exits)
	}
	for _, n := range []string{tcTargetA, tcTargetB} {
		st, err := os.Stat(filepath.Join(repo, n))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s not stripped: %o", n, st.Mode().Perm())
		}
	}
}

// TestLockedFile_WriteByThirdPartyReturnsEACCES pins the chmod-strip enforcement:
// after lock, a non-loto writer (third-party tool, hostile script) must hit EACCES.
// This is the whole point of the lockout primitive.
func TestLockedFile_WriteByThirdPartyReturnsEACCES(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod write bits; EACCES is unreachable")
	}
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, tcTargetX)
	if err := os.WriteFile(p, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdLock, tcTargetX, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	err := os.WriteFile(p, []byte("clobber"), 0o644)
	if err == nil {
		t.Fatal("expected EACCES, got nil")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("expected fs.ErrPermission, got %v", err)
	}
}

func TestLockedFile_StillReadable(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, tcTargetX)
	if err := os.WriteFile(p, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdLock, tcTargetX, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read locked file: %v", err)
	}
	if string(got) != "orig" {
		t.Errorf("got %q", got)
	}
}

func TestUnlock_RestoresOwnerWrite(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, tcTargetX)
	if err := os.WriteFile(p, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdLock, tcTargetX, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	if code := Run([]string{tcCmdUnlock, tcTargetX, "-t", tcIntentDone}, io.Discard, io.Discard); code != 0 {
		t.Fatal("unlock")
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("expected owner-write restored, got %o", st.Mode().Perm())
	}
	if err := os.WriteFile(p, []byte("ok"), 0o644); err != nil {
		t.Errorf("write after unlock failed: %v", err)
	}
}

func TestUnlock_FileDeletedWhileHeldIsNoErrorOnRestore(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, tcTargetX)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdLock, tcTargetX, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdUnlock, tcTargetX, "-t", tcIntentDone}, io.Discard, io.Discard); code != 0 {
		t.Fatal("unlock should succeed even when file was deleted while held")
	}
}

// TestDoctor_CrashRecoveryRoundTrip exercises the orphan-mode flow: scan
// reports without repair; --repair --restore-orphan-mode actually restores.
func TestDoctor_CrashRecoveryRoundTrip(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(p, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdDoctor, tcFlagOrphan}, &out, io.Discard); code != 0 {
		t.Fatalf("doctor scan: %s", out.String())
	}
	if !strings.Contains(out.String(), "orphan-mode") {
		t.Errorf("missing orphan-mode: %s", out.String())
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 != 0 {
		t.Errorf("flag-only should not restore: %o", st.Mode().Perm())
	}
	if code := Run([]string{tcCmdDoctor, tcFlagRepair, "--restore-orphan-mode"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("repair")
	}
	st, err = os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("expected restored: %o", st.Mode().Perm())
	}
}

// TestLockedFile_ChmodPlusWAllowsWrite documents the threat-model bypass:
// loto defeats cooperating Claudes; an actor with chmod can clobber. This is
// expected — the test fails if someone "fixes" it without updating the model.
func TestLockedFile_ChmodPlusWAllowsWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass; precondition undefined")
	}
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, tcTargetX)
	if err := os.WriteFile(p, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdLock, tcTargetX, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("clobber"), 0o644); err != nil {
		t.Errorf("expected write to succeed after chmod +w, got: %v", err)
	}
}
