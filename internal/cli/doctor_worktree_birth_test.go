package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// worktreeAdminDirFor returns the common dir's worktrees/<name> path for
// repo — a fresh repo from withTempProject, whose common dir is repo/.git.
func worktreeAdminDirFor(repo, name string) string {
	return filepath.Join(repo, ".git", refWorktreesDir, name)
}

// plantUnbornWorktreeDir creates a worktree admin dir with the exact
// mid-birth shape worktreeUnborn keys on — HEAD.lock naming branch, a
// `locked` marker, and no HEAD file — then backdates HEAD.lock's mtime by
// age so scanStaleUnbornWorktrees reads the birth as having started that
// long ago.
func plantUnbornWorktreeDir(t *testing.T, repo, name, branch string, age time.Duration) string {
	t.Helper()
	dir := worktreeAdminDirFor(repo, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, refHeadLockFile)
	if err := os.WriteFile(lockPath, []byte("ref: refs/heads/"+branch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, refWorktreeLockedFile), []byte("initializing"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(lockPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDoctorReportsStaleUnbornWorktree is loto-rode's central AC: a worktree
// admin dir mid-birth longer than worktreeBirthStaleAfter is reported by
// name, target branch, age, and a fix block.
func TestDoctorReportsStaleUnbornWorktree(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)

	plantUnbornWorktreeDir(t, repo, "ghost", "feature-x", worktreeBirthStaleAfter+time.Minute)

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "⚠ stale_unborn_worktrees count=1") {
		t.Fatalf("expected one stale unborn worktree row: %q", out)
	}
	wantDir := "dir=" + filepath.Join(".git", refWorktreesDir, "ghost")
	if !strings.Contains(out, wantDir) {
		t.Errorf("row must name the admin dir (%s): %q", wantDir, out)
	}
	if !strings.Contains(out, "branch=feature-x") {
		t.Errorf("row must name the target branch: %q", out)
	}
	if !strings.Contains(out, "age=") {
		t.Errorf("row must carry an age: %q", out)
	}
	if !strings.Contains(out, "rm -rf") || !strings.Contains(out, "git worktree prune") {
		t.Errorf("row must carry the manual-clear fix block: %q", out)
	}
}

// TestDoctorSkipsFreshUnbornWorktree is loto-rode's other AC half: a worktree
// dir mid-birth younger than the threshold is not cruft, and must not be
// reported at all — Rules forbid reaping (or even flagging) one while a
// `git worktree add` could still be running.
func TestDoctorSkipsFreshUnbornWorktree(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)

	plantUnbornWorktreeDir(t, repo, "midflight", "feature-y", time.Second)

	out := runOK(t, tcCmdDoctor)
	if strings.Contains(out, "stale_unborn_worktrees") {
		t.Errorf("a fresh mid-birth worktree dir must not be reported: %q", out)
	}
}

// TestDoctorStaleUnbornWorktreeRowByteIdenticalAcrossRuns is the golden-test
// requirement doctor_stash_test.go's sibling exercises: same repo state, two
// reads a moment apart, same row modulo the wall-clock age field.
func TestDoctorStaleUnbornWorktreeRowByteIdenticalAcrossRuns(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	plantUnbornWorktreeDir(t, repo, "ghost", "feature-x", worktreeBirthStaleAfter+time.Minute)

	first := normalizeAge(runOK(t, tcCmdDoctor))
	second := normalizeAge(runOK(t, tcCmdDoctor))
	if first != second {
		t.Errorf("doctor's stale-unborn-worktree row must be stable across runs on unchanged state:\n first  %q\n second %q", first, second)
	}
}

// TestDoctorRepairNeverTouchesUnbornWorktree is loto-rode's Rules half that
// AC does not restate explicitly but the Story does: reaping one is never
// automatic. --repair must leave the admin dir exactly as found.
func TestDoctorRepairNeverTouchesUnbornWorktree(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	dir := plantUnbornWorktreeDir(t, repo, "ghost", "feature-x", worktreeBirthStaleAfter+time.Minute)

	runOK(t, tcCmdDoctor, tcFlagRepair)

	if !worktreeUnborn(dir) {
		t.Fatalf("--repair must not touch a stale unborn worktree dir: %s no longer reads as unborn", dir)
	}
}

// TestWorktreeUnbornPredicate covers the four presence/absence combinations
// worktreeUnborn discriminates between (mirrors cmd_hook_ref.go's headWorktreeBirth
// carve-out, loto-w0sx — see doctor_worktree_birth.go's package comment).
func TestWorktreeUnbornPredicate(t *testing.T) {
	tests := []struct {
		name       string
		writeHead  bool
		writeLock  bool
		wantUnborn bool
	}{
		{"head absent, locked present: unborn", false, true, true},
		{"head present, locked present: a finished worktree someone locked", true, true, false},
		{"head absent, locked absent: not a worktree admin dir at all", false, false, false},
		{"head present, locked absent: an ordinary finished worktree", true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.writeHead {
				if err := os.WriteFile(filepath.Join(dir, refHEAD), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.writeLock {
				if err := os.WriteFile(filepath.Join(dir, refWorktreeLockedFile), []byte("initializing"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := worktreeUnborn(dir); got != tt.wantUnborn {
				t.Errorf("worktreeUnborn() = %v, want %v", got, tt.wantUnborn)
			}
		})
	}
}
