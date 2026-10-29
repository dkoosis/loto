package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
)

// fakeLinkedCheckout lays out a linked checkout as `git worktree add` does:
// <common>/worktrees/<name>/commondir, and <top>/.git naming that admin dir.
func fakeLinkedCheckout(t *testing.T, common, name string) string {
	t.Helper()
	admin := filepath.Join(common, worktreesDir, name)
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	top := t.TempDir()
	if err := os.WriteFile(filepath.Join(top, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return top
}

func TestWorktreeKey(t *testing.T) {
	primary := t.TempDir()
	common := filepath.Join(primary, ".git")
	if err := os.Mkdir(common, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := fakeLinkedCheckout(t, common, "agent-a")

	submodule := t.TempDir()
	modDir := filepath.Join(common, "modules", "sub")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(submodule, ".git"), []byte("gitdir: "+modDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, top, want string }{
		{"primary checkout", primary, WorktreeMain},
		{"linked checkout", linked, tcWorktreeAgentA},
		{"submodule's own checkout", submodule, WorktreeMain},
		{"no checkout there", filepath.Join(primary, "gone"), ""},
		{"unset", "", ""},
	} {
		if got := WorktreeKey(tc.top); got != tc.want {
			t.Errorf("%s: WorktreeKey(%q) = %q, want %q", tc.name, tc.top, got, tc.want)
		}
	}
}

// TestOpen_KeysPathStampedHoldsByAdminDir: rows a prior loto stamped with an
// absolute checkout path are rekeyed on open (loto-hu8o). A path that still
// holds a checkout takes its WorktreeKey; a path that no longer does becomes
// "" (unknown), never an inert key no checkout matches.
func TestOpen_KeysPathStampedHoldsByAdminDir(t *testing.T) {
	common := filepath.Join(t.TempDir(), ".git")
	linked := fakeLinkedCheckout(t, common, "agent-a")
	gone := filepath.Join(t.TempDir(), "moved-away")
	dbPath := filepath.Join(t.TempDir(), "loto.db")
	ctx := context.Background()

	s, err := OpenContext(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []struct{ name, stamp string }{{"a.go", linked}, {"b.go", gone}} {
		lock := mkFileLockSessionWorktree(t, l.name, tcAlice, "s", "", l.stamp, time.Hour)
		if _, err := s.AcquireLocks(ctx, []domain.LockRecord{lock}, liveProbe); err != nil {
			t.Fatal(err)
		}
	}
	c := mkClaim(tcPkgStore, tcAlice, time.Hour)
	c.Worktree = linked
	if err := s.ClaimPrefix(ctx, c, nil); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = OpenContext(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	locks, err := s.ListLocks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range locks {
		got[filepath.Base(l.Target.Canonical)] = l.Worktree
	}
	if got["a.go"] != tcWorktreeAgentA || got["b.go"] != "" || len(got) != 2 {
		t.Errorf("want a.go keyed by its admin dir and b.go unknown, got %+v", got)
	}
	var claimWt string
	if err := s.db.QueryRowContext(ctx, `SELECT worktree FROM claims`).Scan(&claimWt); err != nil {
		t.Fatal(err)
	}
	if claimWt != tcWorktreeAgentA {
		t.Errorf("want the claim keyed by its admin dir, got %q", claimWt)
	}
}
