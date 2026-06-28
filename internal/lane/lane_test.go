package lane

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- harness ----------------------------------------------------------------

// gitT runs git in dir with a fixed identity, failing the test on error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=base", "GIT_AUTHOR_EMAIL=base@t",
		"GIT_COMMITTER_NAME=base", "GIT_COMMITTER_EMAIL=base@t",
	)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	addBase   = "package calc\n\nfunc Add(a, b int) int { return a + b }\n"
	addEdited = "package calc\n\nfunc Add(a, b int) int { return a + b }\nfunc Sub(a, b int) int { return a - b }\n"
	mulBase   = "package calc\n\nfunc Mul(a, b int) int { return a * b }\n"
	// mulBroken references an undefined symbol — a peer lane's half-finished edit.
	mulBroken = "package calc\n\nfunc Mul(a, b int) int { return a * b }\nfunc Square(a int) int { return a * undefinedHelper(a) }\n"
)

// newBaseRepo creates a repo with go.mod, add.go, mul.go, calc_test.go committed
// and returns the working-tree root and the base commit SHA. Git config is
// isolated so a developer's global ~/.gitconfig (gpgsign, templates) cannot
// perturb the plumbing under test or the lane commits it builds.
func newBaseRepo(t *testing.T) (repoTop, base string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	// Fixed dates make lane commit SHAs deterministic (with the fixed identity),
	// which the reproducibility test relies on. commit-tree honors these.
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-01T00:00:00Z")

	repoTop = t.TempDir()
	gitT(t, repoTop, "init", "-q", "-b", "main")
	gitT(t, repoTop, "config", "commit.gpgsign", "false")
	writeFile(t, repoTop, "go.mod", "module spike/calc\n\ngo 1.21\n")
	writeFile(t, repoTop, "add.go", addBase)
	writeFile(t, repoTop, "mul.go", mulBase)
	writeFile(t, repoTop, "calc_test.go", "package calc\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "base")
	return repoTop, gitT(t, repoTop, "rev-parse", "HEAD")
}

func laneOpts(repoTop, base, ref string, writeSet ...string) Opts {
	return Opts{
		RepoTop:   repoTop,
		Base:      base,
		Ref:       ref,
		WriteSet:  writeSet,
		Message:   "loto/" + ref + "\n\nCloses: none\n",
		Author:    Identity{Name: "loto", Email: "loto@test"},
		Committer: Identity{Name: "loto", Email: "loto@test"},
	}
}

func mustCommit(t *testing.T, opts Opts) string {
	t.Helper()
	sha, err := Commit(context.Background(), opts)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return sha
}

// --- the load-bearing isolation claims (port of spike-loto-plumbing.sh) -----

// TestCommitIsolatesWriteSetFromDirtyUnion is the core thesis: two lanes edit
// one package in one shared tree; lane A's commit must carry ONLY A's write-set
// even though B's broken edit pollutes the shared disk, with HEAD parked and the
// shared index untouched.
func TestCommitIsolatesWriteSetFromDirtyUnion(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	// Both lanes dirty the shared working tree.
	writeFile(t, repoTop, "add.go", addEdited) // lane A: valid
	writeFile(t, repoTop, "mul.go", mulBroken) // lane B: half-finished, won't compile

	a := mustCommit(t, laneOpts(repoTop, base, "A", "add.go"))

	if got := gitT(t, repoTop, "show", a+":add.go"); !strings.Contains(got, "func Sub") {
		t.Errorf("lane A tree missing A's edit (Sub):\n%s", got)
	}
	if got := gitT(t, repoTop, "show", a+":mul.go"); strings.Contains(got, "Square") {
		t.Errorf("lane A tree bled B's broken edit into mul.go:\n%s", got)
	}
	if got := gitT(t, repoTop, "show", a+":mul.go"); got != strings.TrimRight(mulBase, "\n") {
		t.Errorf("lane A mul.go != base:\n%s", got)
	}
	if head := gitT(t, repoTop, "rev-parse", "HEAD"); head != base {
		t.Errorf("HEAD moved: %s != base %s", head, base)
	}
	if ref := gitT(t, repoTop, "rev-parse", "refs/heads/loto/A"); ref != a {
		t.Errorf("ref refs/heads/loto/A = %s, want %s", ref, a)
	}
	if staged := gitT(t, repoTop, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("shared index was mutated; staged paths: %q", staged)
	}
}

// TestCommitSeedsFromParentNotEmpty guards the read-tree-from-base step: files
// the lane did NOT touch must survive in its tree. An empty-index seed would
// drop them, turning the commit into a mass delete.
func TestCommitSeedsFromParentNotEmpty(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, "add.go", addEdited)

	a := mustCommit(t, laneOpts(repoTop, base, "A", "add.go"))

	tree := gitT(t, repoTop, "ls-tree", "-r", "--name-only", a)
	for _, want := range []string{"go.mod", "add.go", "mul.go", "calc_test.go"} {
		if !strings.Contains(tree, want) {
			t.Errorf("lane tree dropped untouched file %q; tree:\n%s", want, tree)
		}
	}
}

// TestCommitExcludesUntrackedChurn ports spike-daemon-dirty.sh: a tracked file
// rewritten on disk but NOT in the write-set must commit at its BASE value — the
// lane stages only its write-set, so a churning daemon file cannot leak in.
func TestCommitExcludesUntrackedChurn(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, "add.go", addEdited)
	// Simulate the daemon: rewrite a tracked file the lane does not own.
	writeFile(t, repoTop, "go.mod", "module spike/calc\n\ngo 1.21\n// daemon-churn marker\n")

	a := mustCommit(t, laneOpts(repoTop, base, "A", "add.go"))

	laneMod := gitT(t, repoTop, "show", a+":go.mod")
	baseMod := gitT(t, repoTop, "show", base+":go.mod")
	if laneMod != baseMod {
		t.Errorf("daemon churn leaked into lane commit:\nlane:\n%s\nbase:\n%s", laneMod, baseMod)
	}
}

// TestCommitMultiWaveParentsOnTip checks the multi-wave path: a second Commit on
// the same lane parents on the lane tip (not base) and its tree accumulates both
// write-sets.
func TestCommitMultiWaveParentsOnTip(t *testing.T) {
	repoTop, base := newBaseRepo(t)

	writeFile(t, repoTop, "add.go", addEdited)
	first := mustCommit(t, laneOpts(repoTop, base, "A", "add.go"))

	writeFile(t, repoTop, "mul.go", "package calc\n\nfunc Mul(a, b int) int { return a * b }\nfunc Sq(a int) int { return Mul(a, a) }\n")
	second := mustCommit(t, laneOpts(repoTop, base, "A", "mul.go"))

	if parent := gitT(t, repoTop, "rev-parse", second+"^"); parent != first {
		t.Errorf("second commit parent = %s, want first %s", parent, first)
	}
	if got := gitT(t, repoTop, "show", second+":add.go"); !strings.Contains(got, "func Sub") {
		t.Errorf("second tree lost the first wave's add.go edit:\n%s", got)
	}
	if got := gitT(t, repoTop, "show", second+":mul.go"); !strings.Contains(got, "func Sq") {
		t.Errorf("second tree missing the second wave's mul.go edit:\n%s", got)
	}
}

// TestCommitDeletionInWriteSet confirms -A captures a deletion of a write-set
// path: the file is removed on disk, and the lane tree must not contain it.
func TestCommitDeletionInWriteSet(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	if err := os.Remove(filepath.Join(repoTop, "mul.go")); err != nil {
		t.Fatal(err)
	}

	a := mustCommit(t, laneOpts(repoTop, base, "A", "mul.go"))

	tree := gitT(t, repoTop, "ls-tree", "-r", "--name-only", a)
	if strings.Contains(tree, "mul.go") {
		t.Errorf("deleted write-set path still present in lane tree:\n%s", tree)
	}
	if !strings.Contains(tree, "add.go") {
		t.Errorf("deletion of mul.go wrongly dropped untouched add.go:\n%s", tree)
	}
}

// TestCommitDeterministic checks byte-identical SHAs for identical inputs under
// the fixed identity+date harness — the plumbing adds no nondeterminism of its
// own.
func TestCommitDeterministic(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, "add.go", addEdited)
	first := mustCommit(t, laneOpts(repoTop, base, "A", "add.go"))

	repoTop2, base2 := newBaseRepo(t)
	writeFile(t, repoTop2, "add.go", addEdited)
	second := mustCommit(t, laneOpts(repoTop2, base2, "A", "add.go"))

	if first != second {
		t.Errorf("identical inputs produced different SHAs: %s != %s", first, second)
	}
}

// --- validation -------------------------------------------------------------

func TestCommitValidation(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, "add.go", addEdited)

	good := laneOpts(repoTop, base, "A", "add.go")
	mutate := func(f func(o *Opts)) Opts {
		o := good
		f(&o)
		return o
	}

	cases := []struct {
		name string
		opts Opts
	}{
		{"empty RepoTop", mutate(func(o *Opts) { o.RepoTop = "" })},
		{"empty Base", mutate(func(o *Opts) { o.Base = "" })},
		{"empty Message", mutate(func(o *Opts) { o.Message = "" })},
		{"empty Ref", mutate(func(o *Opts) { o.Ref = "" })},
		{"leading-dash Ref", mutate(func(o *Opts) { o.Ref = "-x" })},
		{"dotdot Ref", mutate(func(o *Opts) { o.Ref = "a..b" })},
		{"bad-char Ref", mutate(func(o *Opts) { o.Ref = "a b" })},
		{"empty WriteSet", mutate(func(o *Opts) { o.WriteSet = nil })},
		{"absolute path", mutate(func(o *Opts) { o.WriteSet = []string{"/etc/passwd"} })},
		{"traversal path", mutate(func(o *Opts) { o.WriteSet = []string{"../escape"} })},
		{"NUL path", mutate(func(o *Opts) { o.WriteSet = []string{"a\x00b"} })},
		{"missing author", mutate(func(o *Opts) { o.Author = Identity{} })},
		{"missing committer email", mutate(func(o *Opts) { o.Committer = Identity{Name: "x"} })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Commit(context.Background(), tc.opts); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
			// A rejected commit must not have created the lane ref.
			if out, err := exec.Command("git", "-C", repoTop, "rev-parse", "--verify", "--quiet", "refs/heads/loto/A").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
				t.Errorf("%s: invalid Commit still wrote a lane ref", tc.name)
			}
		})
	}
}

func TestCommitBadBaseErrors(t *testing.T) {
	repoTop, _ := newBaseRepo(t)
	writeFile(t, repoTop, "add.go", addEdited)
	opts := laneOpts(repoTop, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "A", "add.go")
	if _, err := Commit(context.Background(), opts); err == nil {
		t.Error("expected error for nonexistent base, got nil")
	}
}
