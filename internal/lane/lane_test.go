package lane

import (
	"bytes"
	"context"
	"errors"
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

// --- write-set hardening (loto-zt0l) ----------------------------------------

// TestValidateWriteSetRejects covers the fs84 sweep vectors a literal-path
// write-set must refuse: a directory (bare or trailing-slash), a glob, and
// pathspec magic. Each must surface errInvalidWriteSet so a peer's dirty edits
// can never enter the lane tree via a widened pathspec.
func TestValidateWriteSetRejects(t *testing.T) {
	repoTop, _ := newBaseRepo(t)
	writeFile(t, repoTop, "internal/x.go", "package internal\n") // a real on-disk dir
	cases := []struct {
		name string
		path string
	}{
		{"existing directory", "internal"},
		{"trailing slash dir", "internal/"},
		{"trailing slash file", "add.go/"},
		{"glob star", "*.go"},
		{"glob question", "mu?.go"},
		{"glob bracket", "m[au]l.go"},
		{"magic exclude", ":(exclude)mul.go"},
		{"magic leading colon", ":mul.go"},
		{"magic from-top", ":/mul.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWriteSet(repoTop, []string{tc.path})
			if !errors.Is(err, errInvalidWriteSet) {
				t.Errorf("validateWriteSet(%q) = %v, want errInvalidWriteSet", tc.path, err)
			}
		})
	}
}

// TestValidateWriteSetAcceptsLiteralFiles confirms the hardening does not reject
// legitimate inputs: an existing file, a nested existing file, and a path absent
// from disk (a deletion the lane records under `git add -A`).
func TestValidateWriteSetAcceptsLiteralFiles(t *testing.T) {
	repoTop, _ := newBaseRepo(t)
	writeFile(t, repoTop, "internal/x.go", "package internal\n")
	for _, p := range []string{"add.go", "mul.go", "internal/x.go", "removed-on-disk.go"} {
		if err := validateWriteSet(repoTop, []string{p}); err != nil {
			t.Errorf("validateWriteSet(%q) rejected a legit literal path: %v", p, err)
		}
	}
}

// TestLiteralPathspecDoesNotStopDirExpansion documents WHY validateWriteSet must
// reject directories explicitly: even wrapped in :(literal), a directory pathspec
// still prefix-matches every file under it. buildLaneTree is exercised directly,
// bypassing validateWriteSet, to prove the raw git behavior the validator guards
// against — :(literal) alone is not enough.
func TestLiteralPathspecDoesNotStopDirExpansion(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, "pkg/a.go", "package pkg\n")
	writeFile(t, repoTop, "pkg/b.go", "package pkg\n")

	g := gitRunner{repoTop: repoTop}
	ctx := context.Background()
	parent, err := g.resolveParent(ctx, "X", base)
	if err != nil {
		t.Fatalf("resolveParent: %v", err)
	}
	tree, err := g.buildLaneTree(ctx, "X", parent, []string{"pkg"})
	if err != nil {
		t.Fatalf("buildLaneTree: %v", err)
	}
	out := gitT(t, repoTop, "ls-tree", "-r", "--name-only", tree)
	if !strings.Contains(out, "pkg/a.go") || !strings.Contains(out, "pkg/b.go") {
		t.Fatalf(":(literal) on a directory should still prefix-expand to its files; tree:\n%s", out)
	}
}

// TestCommitDeletionCommitsAsDeletionUnderLiteral is the regression guard for the
// :(literal) wrapping: a write-set path removed from disk must still record as a
// deletion (status D) against the parent, not silently persist at its base blob.
func TestCommitDeletionCommitsAsDeletionUnderLiteral(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	if err := os.Remove(filepath.Join(repoTop, "mul.go")); err != nil {
		t.Fatal(err)
	}

	a := mustCommit(t, laneOpts(repoTop, base, "A", "mul.go"))

	status := gitT(t, repoTop, "diff", "--name-status", base, a)
	if !strings.Contains(status, "D\tmul.go") {
		t.Errorf("mul.go should record as a deletion (D) vs base under :(literal); got:\n%s", status)
	}
}

// TestCommitRejectsRemovedTrackedDirectory is the loto-6uzn regression guard.
// The zt0l directory guard only os.Stat's the worktree, so a write-set entry
// naming a tracked directory REMOVED from disk ('rm -rf pkg') returns ENOENT and
// passes validateWriteSet — then buildLaneTree's :(literal)pkg prefix-expands
// against the parent-seeded index and stages a deletion for EVERY file under
// pkg/ (the fs84 sweep, the symmetric index/HEAD case codex caught on #201). The
// fixed behavior rejects that entry with errInvalidWriteSet before staging. The
// control half proves the fix does not over-reject: a removed single FILE in the
// same shape still stages as one legitimate deletion.
func TestCommitRejectsRemovedTrackedDirectory(t *testing.T) {
	repoTop, _ := newBaseRepo(t)
	// Commit a tracked directory, then remove it from disk entirely.
	writeFile(t, repoTop, "pkg/a.go", "package pkg\n")
	writeFile(t, repoTop, "pkg/b.go", "package pkg\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "add pkg")
	base := gitT(t, repoTop, "rev-parse", "HEAD")
	if err := os.RemoveAll(filepath.Join(repoTop, "pkg")); err != nil {
		t.Fatal(err)
	}

	// The sweep vector: the write-set names the removed tracked directory. It must
	// be rejected, not silently staged as two deletions of pkg/a.go + pkg/b.go.
	if _, err := Commit(context.Background(), laneOpts(repoTop, base, "A", "pkg")); !errors.Is(err, errInvalidWriteSet) {
		t.Errorf("Commit(WriteSet:[pkg]) for a removed tracked dir = %v, want errInvalidWriteSet", err)
	}
	if ref, _ := exec.Command("git", "-C", repoTop, "rev-parse", "--verify", "--quiet", "refs/heads/loto/A").Output(); strings.TrimSpace(string(ref)) != "" {
		t.Errorf("rejected sweep still wrote a lane ref")
	}

	// Control: a removed single FILE still commits as exactly one deletion — the
	// fix must distinguish a tracked dir (reject) from a tracked file (allow).
	if err := os.Remove(filepath.Join(repoTop, "mul.go")); err != nil {
		t.Fatal(err)
	}
	f := mustCommit(t, laneOpts(repoTop, base, "F", "mul.go"))
	status := gitT(t, repoTop, "diff", "--name-status", base, f)
	if !strings.Contains(status, "D\tmul.go") {
		t.Errorf("removed file should still record as a deletion (D); got:\n%s", status)
	}
	// The deletion must touch only mul.go — pkg/ survives untouched (no sweep).
	if other := gitT(t, repoTop, "show", f+":pkg/a.go"); !strings.Contains(other, "package pkg") {
		t.Errorf("the file-deletion lane wrongly dropped untouched pkg/a.go:\n%s", other)
	}
}

// TestCommitRejectsFileShadowingTrackedDirectory is the codex follow-up to
// loto-6uzn: the sibling of the removed-directory vector. A lane replaces a
// tracked directory with a regular file at the same path ('rm -rf pkg; echo x >
// pkg') and names it in the write-set. os.Stat now SUCCEEDS (pkg is a file), so a
// presence-gated probe would skip the index check — yet :(literal)pkg still
// prefix-expands against the parent-seeded index, staging the new file PLUS a
// deletion for every pkg/* file. Empirically confirmed: A pkg, D pkg/a.go, D
// pkg/b.go. The probe must run regardless of on-disk presence; this guards that.
func TestCommitRejectsFileShadowingTrackedDirectory(t *testing.T) {
	repoTop, _ := newBaseRepo(t)
	writeFile(t, repoTop, "pkg/a.go", "package pkg\n")
	writeFile(t, repoTop, "pkg/b.go", "package pkg\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "add pkg")
	base := gitT(t, repoTop, "rev-parse", "HEAD")

	// Replace the tracked directory with a regular file at the same path.
	if err := os.RemoveAll(filepath.Join(repoTop, "pkg")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repoTop, "pkg", "x\n")

	if _, err := Commit(context.Background(), laneOpts(repoTop, base, "A", "pkg")); !errors.Is(err, errInvalidWriteSet) {
		t.Errorf("Commit(WriteSet:[pkg]) for a file shadowing a tracked dir = %v, want errInvalidWriteSet", err)
	}
	if ref, _ := exec.Command("git", "-C", repoTop, "rev-parse", "--verify", "--quiet", "refs/heads/loto/A").Output(); strings.TrimSpace(string(ref)) != "" {
		t.Errorf("rejected sweep still wrote a lane ref")
	}
}

// TestCommitAllowsSubmoduleRemoval is the codex #202 P2 guard: the tracked-dir
// probe must not over-reject a legitimate submodule removal. A gitlink (index
// mode 160000) at path `pkg` matches `ls-files :(literal)pkg/` (its entry path
// equals the dir pathspec), yet `git add -A :(literal)pkg` stages only a single
// `D pkg` — no directory sweep. Empirically confirmed. So naming a removed
// submodule in a write-set is legitimate and must be ALLOWED; only a tracked
// entry STRICTLY UNDER pkg/ is the sweep vector. The gitlink is built directly
// via update-index --cacheinfo to stay hermetic (no submodule protocol flags).
func TestCommitAllowsSubmoduleRemoval(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	// Record a gitlink at `pkg` pointing at any valid commit (base itself works).
	gitT(t, repoTop, "update-index", "--add", "--cacheinfo", "160000,"+base+",pkg")
	gitT(t, repoTop, "commit", "-qm", "add submodule pkg")
	subBase := gitT(t, repoTop, "rev-parse", "HEAD")
	// Remove the (never-checked-out) submodule path from disk, name it in the
	// write-set alongside the usual .gitmodules edit shape.
	if err := os.RemoveAll(filepath.Join(repoTop, "pkg")); err != nil {
		t.Fatal(err)
	}

	sha := mustCommit(t, laneOpts(repoTop, subBase, "S", "pkg"))
	status := gitT(t, repoTop, "diff", "--name-status", subBase, sha)
	if !strings.Contains(status, "D\tpkg") {
		t.Errorf("submodule removal should record as a single deletion (D pkg); got:\n%s", status)
	}
	// Untouched siblings survive — the removal touched only the gitlink.
	if other := gitT(t, repoTop, "show", sha+":add.go"); !strings.Contains(other, "func Add") {
		t.Errorf("submodule-removal lane wrongly dropped untouched add.go:\n%s", other)
	}
}
