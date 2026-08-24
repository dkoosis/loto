package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestLotoBaseIsolatesRealHomeState is the sd-kx5 regression test.
//
// LOTO_BASE redirected the lock store (StateDir, paths.go) but not identity's
// state — ~/.loto/{agents,session,peers} — so a caller that set LOTO_BASE
// without ALSO overriding HOME still wrote real files under the live
// ~/.loto. That is exactly the shape of the actual reproducer, sdlc's
// home/hooks/shared-main-guard.test.sh: it sets LOTO_BASE and overrides
// CLAUDE_CODE_SESSION_ID, but never touches HOME, and left
// somebody-else-<pid>.json behind in dk's real ~/.loto/session on every run.
// Any caller that does NOT override CLAUDE_CODE_SESSION_ID either (a bare
// `loto whoami`) wrote into a LIVE session's cache file for real.
//
// Deliberately does NOT set HOME (unlike withTempProject, used by every other
// CLI test in this package): HOME+LOTO_BASE both redirected would pass even
// against the pre-fix code, since HOME alone already moves identity's old
// literal ~/.loto/... paths into the temp dir — the bug only shows when
// LOTO_BASE is the SOLE isolation knob, which is the real-world case this
// test pins. The assertion is a before/after snapshot of the REAL directory
// tree, not merely "files exist under the temp LOTO_BASE" — that weaker
// assertion would have passed on the buggy code too, since loto did also
// write correctly-isolated lock-store files there.
func TestLotoBaseIsolatesRealHomeState(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil || realHome == "" {
		t.Skip("no resolvable $HOME on this machine; nothing to snapshot")
	}
	before := snapshotLotoTree(realHome)

	base := t.TempDir()
	t.Setenv("LOTO_BASE", base)
	t.Setenv("XDG_STATE_HOME", "")
	sid := fmt.Sprintf("sd-kx5-isolation-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
	os.Unsetenv("LOTO_AGENT_ID")

	repo := t.TempDir()
	initBareGitRepo(t, repo)
	cmd := exec.Command("git", "remote", "add", "origin", "git@github.com:test/sd-kx5.git")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	t.Chdir(repo)
	target := "a.txt"
	if err := os.WriteFile(filepath.Join(repo, target), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// whoami: mints/caches identity and records presence — the exact call the
	// SessionStart hook makes on every session, and the write path this bug
	// bypassed LOTO_BASE for.
	var out, errBuf bytes.Buffer
	if code := Run([]string{"whoami"}, &out, &errBuf); code != 0 {
		t.Fatalf("whoami exit %d; out=%q err=%q", code, out.String(), errBuf.String())
	}

	// lock/unlock: the reproducer's actual sequence.
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"lock", target, "-t", "sd-kx5 isolation test"}, &out, &errBuf); code != 0 {
		t.Fatalf("lock exit %d; out=%q err=%q", code, out.String(), errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"unlock", target, "-t", "done"}, &out, &errBuf); code != 0 {
		t.Fatalf("unlock exit %d; out=%q err=%q", code, out.String(), errBuf.String())
	}

	after := snapshotLotoTree(realHome)
	if diff := diffLotoSnapshots(before, after); diff != "" {
		t.Fatalf("LOTO_BASE=%s set, but the REAL ~/.loto changed:\n%s", base, diff)
	}
}

// snapshotLotoTree lists every path under home/.loto, recursively. An absent
// directory (a box with no prior loto activity) yields an empty set rather
// than an error — that is a valid starting state, not a finding.
func snapshotLotoTree(home string) map[string]struct{} {
	root := filepath.Join(home, ".loto")
	out := map[string]struct{}{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort snapshot: a transient stat error narrows this pass, it does not fail the test
		}
		if path == root {
			return nil
		}
		out[path] = struct{}{}
		return nil
	})
	return out
}

// diffLotoSnapshots reports paths present in after but not before, sorted for
// a stable failure message. Files removed by this run's own GC passes are not
// flagged — sd-kx5's concern is stray writes, not the pre-existing reaper.
func diffLotoSnapshots(before, after map[string]struct{}) string {
	var added []string
	for p := range after {
		if _, ok := before[p]; !ok {
			added = append(added, p)
		}
	}
	if len(added) == 0 {
		return ""
	}
	sort.Strings(added)
	return "new paths under ~/.loto:\n  " + strings.Join(added, "\n  ")
}
