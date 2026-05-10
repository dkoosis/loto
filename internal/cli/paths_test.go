package cli

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectSlugFromOriginRemote(t *testing.T) {
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

	got := ProjectSlug(dir)
	if got != tcSlugDKLoto {
		t.Errorf("ProjectSlug = %q; want dkoosis-loto", got)
	}
}

func TestProjectSlugFallsBackToDirName(t *testing.T) {
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
	got := ProjectSlug(dir)
	if got != "myproject" {
		t.Errorf("ProjectSlug = %q; want myproject", got)
	}
}

func TestStateDirRespectsLOTO_BASE(t *testing.T) {
	t.Setenv("LOTO_BASE", "/tmp/override")
	got := StateDir("/anywhere")
	if got != "/tmp/override" {
		t.Errorf("StateDir = %q; want /tmp/override", got)
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
