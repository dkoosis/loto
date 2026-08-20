package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCheckClean(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcTargetA}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "no conflicts") && !strings.Contains(out.String(), "no paths") {
		t.Errorf("expected clean output: %q", out.String())
	}
}

func TestCheckConflictsWithOtherAgent(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	// Durable, live PID → alice's exclusive lock classifies ALIVE, the
	// provably-live case that hard-blocks under the liveness gate (loto-k5el.2).
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcTargetA}, &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ conflicts") || !strings.Contains(out.String(), "blocker=") {
		t.Errorf("expected conflict report: %q", out.String())
	}
}

// loto-d3l: absolute path that lies inside the repo must report the same
// conflict as the equivalent repo-relative path. Previously the CLI swallowed
// ErrRepoEscape from Canonicalize and emitted "✓ no conflicts".
func TestCheckAcceptsAbsolutePathInsideRepo(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid())) // ALIVE holder → hard block
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)

	abs := filepath.Join(repo, tcTargetA)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, abs}, &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ conflicts") || !strings.Contains(out.String(), "blocker=") {
		t.Errorf("expected conflict report for abs path: %q", out.String())
	}
}

// Negative case for normalizeRepoPath: an absolute path that does not lie
// inside the repo must still be rejected as repo-escape (no silent acceptance).
func TestCheckRejectsAbsolutePathOutsideRepo(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, "/etc/hosts"}, &out, &bytes.Buffer{})
	if code != 2 {
		t.Fatalf("expected exit 2, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ invalid") || !strings.Contains(out.String(), "/etc/hosts") {
		t.Errorf("expected invalid report citing /etc/hosts: %q", out.String())
	}
}

// loto-jff (gh#128): `loto check --staged` must run `git diff --cached`
// with cmd.Dir = repoTop so the staged diff comes from the loto-resolved
// repo, not from process cwd. Without the fix, when cwd is outside the
// target repo (worktree handoff, scripted invocation from a tools dir,
// nested launches), the git invocation reads the wrong repo's index and
// silently emits the wrong paths.
//
// This pins loadCheckTargets at the unit level: it must accept repoTop
// and pass it to git, independent of process cwd.
func TestLoadCheckTargets_UsesRepoTopForGitDiff(t *testing.T) {
	// repoA: the target repo with a staged file. Built by hand so the test
	// is independent of withTempProject side effects (cwd/env).
	repoA := t.TempDir()
	initBareGitRepo(t, repoA)
	stagedRel := filepath.Join("internal", "store", "store.go")
	if err := os.MkdirAll(filepath.Join(repoA, "internal", "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoA, stagedRel), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", stagedRel)
	cmd.Dir = repoA
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("repoA git add: %v\n%s", err, out)
	}

	// cwd points elsewhere (a non-git directory). Without the fix,
	// loadCheckTargets would inherit this cwd and `git diff --cached` would
	// fail (or read whatever ambient repo it discovers).
	cwd := t.TempDir()
	t.Chdir(cwd)

	var stderr bytes.Buffer
	paths, code := loadCheckTargets(t.Context(), repoA, true, nil, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr.String())
	}
	if len(paths) != 1 || filepath.ToSlash(paths[0]) != filepath.ToSlash(stagedRel) {
		t.Fatalf("expected staged path %q, got %v", stagedRel, paths)
	}
}

// loto-9t0q: a TTL-expired holder is reclaimable — `loto lock` would silently
// reclaim it via reclaimStaleAndCollectBlockers / domain.IsStale. The advisory
// `check` gate must agree: it must NOT report an expired lock as a hard
// conflict (exit 1) that demands `unlock --force`. Before the fix, check did no
// staleness filtering and emitted `✗ conflicts count=1` + a force fix-block.
func TestCheckIgnoresExpiredHolder(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	// 1ms TTL: lapses immediately, same shape as a short-claim that timed out.
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagTTL, tcTTL1ms, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	time.Sleep(20 * time.Millisecond)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcTargetA}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("expected exit 0 (reclaimable, not a hard conflict), got %d: %q", code, out.String())
	}
	if strings.Contains(out.String(), "✗ conflicts") {
		t.Errorf("expired holder must not read as a hard conflict: %q", out.String())
	}
}

// loto-9t0q: a holder whose PID is provably dead on this host is reclaimable —
// domain.IsStale returns true via the live probe. `check` must build the same
// liveProbe AcquireLocks uses and not report the dead-PID holder as a hard
// conflict. LOTO_PID lets alice stamp a non-existent PID onto the lock.
func TestCheckIgnoresDeadPidHolder(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	// A PID that is reliably dead: spawn `true`, wait for it to exit, reuse its
	// PID. The OS will not have recycled it within the test window.
	deadPID := spawnAndReap(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", deadPID)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagTTL, "10m", "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	os.Unsetenv("LOTO_PID")

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcTargetA}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("expected exit 0 (dead-PID reclaimable, not a hard conflict), got %d: %q", code, out.String())
	}
	if strings.Contains(out.String(), "✗ conflicts") {
		t.Errorf("dead-PID holder must not read as a hard conflict: %q", out.String())
	}
}

// spawnAndReap runs a short-lived process, waits for it to exit, and returns its
// (now-dead) PID as a string. identity.PIDAlive(pid) reports it dead on this host.
func spawnAndReap(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	return strconv.Itoa(pid)
}

// --- loto-qoq: foreign-claim advisory on plain check ---

// TestCheck_UnderForeignClaim_NoLockConflict_EmitsAdvisory is the P1 case
// (plan): the dominant path — a target under a foreign claim with no lock
// conflict — returns at the len(rows)==0 "✓ no conflicts" early return. The
// advisory must still emit there, not only after printCheckConflicts.
func TestCheck_UnderForeignClaim_NoLockConflict_EmitsAdvisory(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentAliceClaim}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcStoreStoreGo}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "✓ no conflicts") {
		t.Errorf("expected no-conflicts line: %q", s)
	}
	if !strings.Contains(s, "⚠ under-claim count=1") || !strings.Contains(s, "owner="+alice.UUID) {
		t.Errorf("expected foreign-claim advisory: %q", s)
	}
}

// TestCheck_UnderOwnClaim_NoAdvisory: the checker's own claim must not
// advise against itself.
func TestCheck_UnderOwnClaim_NoAdvisory(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("claim failed")
	}
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcStoreStoreGo}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	if strings.Contains(out.String(), "under-claim") {
		t.Errorf("own claim must not emit advisory: %q", out.String())
	}
}

// TestCheck_UnderExpiredClaim_NoAdvisory: a lapsed foreign claim must not
// advise.
func TestCheck_UnderExpiredClaim_NoAdvisory(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest, tcFlagTTL, tcTTL1ms}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim failed")
	}
	time.Sleep(20 * time.Millisecond)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcStoreStoreGo}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	if strings.Contains(out.String(), "under-claim") {
		t.Errorf("expired claim must not emit advisory: %q", out.String())
	}
}

// TestCheck_LockConflictAndForeignClaim_BothRowsExitReflectsLockOnly: a
// target both lock-blocked and under a foreign claim prints conflict rows
// then the advisory block; exit is driven by the lock conflict only.
func TestCheck_LockConflictAndForeignClaim_BothRowsExitReflectsLockOnly(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid())) // ALIVE holder → hard block
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentAliceClaim}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcStoreStoreGo}, &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("expected exit 1 (lock conflict), got %d: %q", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "✗ conflicts") || !strings.Contains(s, "blocker=") {
		t.Errorf("expected lock conflict rows: %q", s)
	}
	if !strings.Contains(s, "⚠ under-claim count=1") {
		t.Errorf("expected foreign-claim advisory alongside conflict: %q", s)
	}
	if strings.Index(s, "✗ conflicts") > strings.Index(s, "under-claim") {
		t.Errorf("expected conflict rows before advisory block: %q", s)
	}
}

// TestCheck_MultiTarget_EachUnderDifferentForeignClaim: two targets, each
// under a different foreign claimant's prefix, each yield one advisory row,
// sorted by target.
func TestCheck_MultiTarget_EachUnderDifferentForeignClaim(t *testing.T) {
	repo := withTempProject(t)
	alice, bob, carol := threeAgents(t)

	renderDir := filepath.Join(repo, "internal", "render")
	if err := os.MkdirAll(renderDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(renderDir, "cli.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentAliceClaim}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim failed")
	}
	t.Setenv("LOTO_AGENT_ID", carol.UUID)
	if code := Run([]string{tcCmdClaim, "internal/render", "-t", "carol-claim"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("carol claim failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcStoreStoreGo, "internal/render/cli.go"}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "⚠ under-claim count=2") {
		t.Errorf("expected two advisory rows: %q", s)
	}
	if !strings.Contains(s, "target="+tcStoreStoreGo) || !strings.Contains(s, "target=internal/render/cli.go") {
		t.Errorf("expected one row per target: %q", s)
	}
	renderIdx := strings.Index(s, "target=internal/render/cli.go")
	storeIdx := strings.Index(s, "target="+tcStoreStoreGo)
	if renderIdx == -1 || storeIdx == -1 || renderIdx > storeIdx {
		t.Errorf("expected target-sorted advisory rows: %q", s)
	}
}

// TestCheck_DuplicateTargetArg_SingleAdvisoryRow: unlike lock, plain check's
// resolveCheckTargets does not dedup its input — "loto check foo foo" must
// still yield a single deduped advisory row via the seen-map guard.
func TestCheck_DuplicateTargetArg_SingleAdvisoryRow(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentAliceClaim}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcStoreStoreGo, tcStoreStoreGo}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "⚠ under-claim count=1") {
		t.Errorf("expected deduped single advisory row for duplicate check arg: %q", s)
	}
	if strings.Count(s, "target="+tcStoreStoreGo) != 1 {
		t.Errorf("expected exactly one advisory row: %q", s)
	}
}

// TestCheck_RelativeTokenFromSubdirFindsPeerConflict is the loto-3tv3 false
// clean, end to end. From internal/store, `loto check store.go` used to key
// `store.go`, find nothing, and print ✓ no conflicts while a peer held
// internal/store/store.go. Invariant 9 calls that strictly worse than no check.
func TestCheck_RelativeTokenFromSubdirFindsPeerConflict(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	t.Chdir(filepath.Join(repo, "internal", "store"))
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcStoreGo}, &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("expected exit 1 (conflict), got %d: %q", code, out.String())
	}
	if strings.Contains(out.String(), "no conflicts") {
		t.Fatalf("false clean: %q", out.String())
	}
	if !strings.Contains(out.String(), "path=internal/store/store.go") {
		t.Errorf("expected the conflict on the file the caller means: %q", out.String())
	}
}

// TestCheck_SameBasenameAtRootDoesNotCollide is the collision case the AC
// names: one token, two cwds, two correct-and-different verdicts.
func TestCheck_SameBasenameAtRootDoesNotCollide(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)

	if err := os.WriteFile(filepath.Join(repo, tcStoreGo), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var rootOut bytes.Buffer
	if code := Run([]string{tcCmdCheck, tcStoreGo}, &rootOut, &bytes.Buffer{}); code != 0 {
		t.Fatalf("root store.go is unheld, want exit 0, got %d: %q", code, rootOut.String())
	}
	t.Chdir(filepath.Join(repo, "internal", "store"))
	var subOut bytes.Buffer
	if code := Run([]string{tcCmdCheck, tcStoreGo}, &subOut, &bytes.Buffer{}); code != 1 {
		t.Fatalf("internal/store/store.go is held, want exit 1, got %d: %q", code, subOut.String())
	}
}

// TestCheckStaged_FromSubdirStillResolvesGitTokens is the --staged fence. git
// produces its paths with cmd.Dir=repoTop, so they are repo-root-relative
// already; re-basing them on the caller's cwd would make every staged check
// from a subdirectory resolve to nonsense. This test fails if a blanket cwd
// join sneaks into the resolver.
func TestCheckStaged_FromSubdirStillResolvesGitTokens(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("add", tcStoreStoreGo)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	t.Chdir(filepath.Join(repo, "internal", "store"))
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, "--staged"}, &out, &bytes.Buffer{})
	if strings.Contains(out.String(), "invalid") {
		t.Fatalf("staged token was re-based on the cwd: %q", out.String())
	}
	if code != 1 || !strings.Contains(out.String(), "path=internal/store/store.go") {
		t.Errorf("expected the staged conflict, got exit %d: %q", code, out.String())
	}
}
