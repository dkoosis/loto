package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveAndPinProjectSlugFromOriginRemote(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "git@github.com:dkoosis/loto.git")

	got := ResolveAndPinProjectSlug(dir)
	if got != tcSlugDKLoto {
		t.Errorf("ResolveAndPinProjectSlug = %q; want dkoosis-loto", got)
	}
}

func TestResolveAndPinProjectSlugFallsBackToDirName(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "myproject")
	if err := exec.Command("mkdir", dir).Run(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	got := ResolveAndPinProjectSlug(dir)
	if got != "myproject" {
		t.Errorf("ResolveAndPinProjectSlug = %q; want myproject", got)
	}
}

func TestStateDirRespectsLOTO_BASE(t *testing.T) {
	t.Setenv("LOTO_BASE", "/tmp/override")
	got := StateDir("/anywhere")
	if got != "/tmp/override" {
		t.Errorf("StateDir = %q; want /tmp/override", got)
	}
}

// loto-d3l (case variant): on a case-insensitive filesystem a repo checked out
// at .../MixedCaseRepo can receive a path with a different case in the segments
// at/above the checkout root — a git worktree minted from a lowercase cwd hands
// loto /Users/x/projects/... while git records /Users/x/Projects/.... Lexical,
// case-sensitive filepath.Rel reports a bogus escape; normalizeRepoPath must
// recover repo-relative containment via a case-insensitive comparison.
//
// Skips on a case-sensitive filesystem, where the case mismatch cannot occur.
func TestNormalizeRepoPath_CaseInsensitiveContainment(t *testing.T) {
	top := t.TempDir()
	repo := filepath.Join(top, "MixedCaseRepo")
	if err := os.MkdirAll(filepath.Join(repo, "pkg", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "pkg", "sub", "file.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	lowerRepo := filepath.Join(top, "mixedcaserepo")
	if _, err := os.Stat(lowerRepo); err != nil {
		t.Skip("case-sensitive filesystem: case-mismatch cannot reproduce")
	}
	lowerFile := filepath.Join(lowerRepo, "pkg", "sub", "file.go")

	got := normalizeRepoPath(lowerFile, repo)
	if got != "pkg/sub/file.go" {
		t.Fatalf("normalizeRepoPath(%q, %q) = %q; want pkg/sub/file.go", lowerFile, repo, got)
	}
}

func TestNormalizeURLVariants(t *testing.T) {
	cases := map[string]string{
		"git@github.com:dkoosis/loto.git":     tcSlugDKLoto,
		"https://github.com/dkoosis/loto":     tcSlugDKLoto,
		"https://github.com/dkoosis/loto.git": tcSlugDKLoto,
		"":                                    unnamedSlug,
	}
	for in, want := range cases {
		got := normalizeURL(in)
		if got != want {
			t.Errorf("normalizeURL(%q) = %q; want %q", in, got, want)
		}
	}
	_ = strings.Builder{}
}
