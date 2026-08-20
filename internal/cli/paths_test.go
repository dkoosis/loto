package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"loto/internal/domain"
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

// TestXdgStateHomeAbsoluteWhenHomeUnset guards against the relative-path
// regression: os.UserHomeDir() failing (empty $HOME) used to return a bare
// ".local/state", silently rooting the per-project store at
// whatever cwd a command happened to run from (loto-7wi). The fallback must
// stay absolute regardless of which rung of the cascade answers.
func TestXdgStateHomeAbsoluteWhenHomeUnset(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	got := xdgStateHome()
	if !filepath.IsAbs(got) {
		t.Errorf("xdgStateHome() = %q; want an absolute path even when $HOME is unset", got)
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

// evalTop resolves symlinks on a temp checkout root so expectations line up
// with what repoRelFromBase computes. t.TempDir() hands back /var/... on macOS
// while the physical path is /private/var/... (paths.go precedent).
func evalTop(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestResolveCLITarget_RelativeTokenUsesCallerBase is the loto-3tv3 core: a
// bare token typed in a subdirectory names the file the caller sees, not the
// same-named file at the repo root.
func TestResolveCLITarget_RelativeTokenUsesCallerBase(t *testing.T) {
	top := evalTop(t, t.TempDir())
	base := filepath.Join(top, "internal", "store")

	got, err := resolveCLITarget(base, top, "locks.go")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Canonical != "internal/store/locks.go" {
		t.Errorf("canonical = %q, want internal/store/locks.go", got.Canonical)
	}
}

// TestResolveCLITarget_RepoRootBaseUnchanged pins the AC clause "repo-root
// invocations unchanged": from the root the caller's base IS the repo top, so
// every key it minted before it mints now.
func TestResolveCLITarget_RepoRootBaseUnchanged(t *testing.T) {
	top := evalTop(t, t.TempDir())
	for _, tc := range []struct{ raw, want string }{
		{tcTargetA, tcTargetA},
		{tcStoreStoreGo, tcStoreStoreGo},
		{"./" + tcTargetA, tcTargetA},
	} {
		got, err := resolveCLITarget(top, top, tc.raw)
		if err != nil {
			t.Errorf("%q: %v", tc.raw, err)
			continue
		}
		if got.Canonical != tc.want {
			t.Errorf("%q: canonical = %q, want %q", tc.raw, got.Canonical, tc.want)
		}
	}
}

// TestResolveCLITarget_StagedBaseIsRepoTop is the --staged fence at the unit
// level: git produces its tokens with cmd.Dir=repoTop, so they are already
// repo-root-relative and must NOT be re-based onto the caller's cwd. A blanket
// cwd join inside the resolver fails here.
func TestResolveCLITarget_StagedBaseIsRepoTop(t *testing.T) {
	top := evalTop(t, t.TempDir())
	sub := filepath.Join(top, "internal", "store")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	got, err := resolveCLITarget(top, top, tcStoreStoreGo)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Canonical != tcStoreStoreGo {
		t.Errorf("canonical = %q, want internal/store/store.go — a git token was re-based on the cwd", got.Canonical)
	}
}

// TestResolveCLITarget_SpellingVerdictsSurviveTheJoin pins the D2 rule:
// spelling is judged on the RAW token, before filepath.Join Cleans it away. A
// trailing slash and a glob metacharacter are the two that would otherwise
// flip verdict — `check sub/` would go from exit 2 to "✓ no conflicts".
func TestResolveCLITarget_SpellingVerdictsSurviveTheJoin(t *testing.T) {
	top := evalTop(t, t.TempDir())
	base := filepath.Join(top, "sub")
	for _, tc := range []struct {
		raw  string
		want error
	}{
		{"", domain.ErrEmptyTarget},
		{"x\x00y", domain.ErrTargetHasNUL},
		{`a\b`, domain.ErrTargetBackslash},
		{"a*.go", domain.ErrTargetIsGlob},
		{"sub/", domain.ErrTargetIsDir},
	} {
		_, err := resolveCLITarget(base, top, tc.raw)
		if !errors.Is(err, tc.want) {
			t.Errorf("%q: err = %v, want %v", tc.raw, err, tc.want)
		}
	}
}

// TestResolveCLITarget_PositionalVerdictsRebasedOnCallerCwd pins the intended
// semantic changes: "is this the repo root" and "does this escape the repo" are
// base-dependent, so they are re-decided after the join. `../cli/paths.go` from
// internal/store is an obviously in-repo path that used to report repo-escape —
// a lie the caller could not act on.
func TestResolveCLITarget_PositionalVerdictsRebasedOnCallerCwd(t *testing.T) {
	top := evalTop(t, t.TempDir())
	store := filepath.Join(top, "internal", "store")

	for _, tc := range []struct {
		name, base, raw, want string
		wantErr               error
	}{
		{name: "dot at root is the repo root", base: top, raw: ".", wantErr: domain.ErrTargetIsRepoRoot},
		{name: "dotdot at root escapes", base: top, raw: "..", wantErr: domain.ErrRepoEscape},
		{name: "dotdot from a subdir names the root", base: store, raw: "../..", wantErr: domain.ErrTargetIsRepoRoot},
		{name: "sibling package resolves", base: store, raw: "../cli/paths.go", want: "internal/cli/paths.go"},
		{name: "deep escape still escapes", base: store, raw: "../../../../etc/passwd", wantErr: domain.ErrRepoEscape},
	} {
		got, err := resolveCLITarget(tc.base, top, tc.raw)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.Canonical != tc.want {
			t.Errorf("%s: canonical = %q, want %q", tc.name, got.Canonical, tc.want)
		}
	}
}

// TestResolveCLITarget_NoRepoTopUnchanged pins D7: outside a git repo there is
// no repo-relative frame, so the resolver must behave exactly as it did before.
func TestResolveCLITarget_NoRepoTopUnchanged(t *testing.T) {
	for _, raw := range []string{tcTargetA, "sub/" + tcTargetA, "", "a*.go"} {
		got, gotErr := resolveCLITarget(t.TempDir(), "", raw)
		want, wantErr := domain.Canonicalize(raw)
		if got != want || (gotErr == nil) != (wantErr == nil) {
			t.Errorf("%q: got (%+v, %v), want (%+v, %v)", raw, got, gotErr, want, wantErr)
		}
	}
}

// TestResolveCLITarget_EmptyBaseRefusesRelative pins D6. A deleted cwd is the
// one case where the base is genuinely unknowable on the direct-CLI path;
// falling back to repo-root-relative would be the false clean invariant 9
// forbids. Absolute tokens carry their own base and are unaffected.
func TestResolveCLITarget_EmptyBaseRefusesRelative(t *testing.T) {
	top := evalTop(t, t.TempDir())

	_, err := resolveCLITarget("", top, tcTargetA)
	if !errors.Is(err, errCallerCWDUnknown) {
		t.Fatalf("err = %v, want errCallerCWDUnknown", err)
	}
	if got := classifyCanonicalizeErr(err); got != "relative-path-caller-cwd-unknown" {
		t.Errorf("reason = %q, want relative-path-caller-cwd-unknown", got)
	}
	got, err := resolveCLITarget("", top, filepath.Join(top, tcTargetA))
	if err != nil {
		t.Fatalf("absolute token with no base: %v", err)
	}
	if got.Canonical != tcTargetA {
		t.Errorf("canonical = %q, want a.go", got.Canonical)
	}
}

// TestResolveCLITarget_RelativeLeafSymlinkStillRefused pins the D4 invariant
// that keeps loto-j39r's scope intact: the resolver joins and contains, but
// never resolves symlinks on the TOKEN. Minting a.go here would sail past the
// symlink refusal in statFileTargetReason and decide j39r's policy question as
// a side effect of a cwd fix.
func TestResolveCLITarget_RelativeLeafSymlinkStillRefused(t *testing.T) {
	top := evalTop(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(top, "a.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(top, tcTargetA), filepath.Join(top, tcTargetSym)); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCLITarget(top, top, tcTargetSym)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Canonical != tcTargetSym {
		t.Errorf("canonical = %q, want sym.go — the token must not be symlink-resolved", got.Canonical)
	}
}

// TestResolveCLITarget_SymlinkedAncestorConverges is the loto-j39r defect-1
// regression. domain.Canonicalize is lexical, so link/a.go and real/a.go named
// one file under two keys — and two agents could hold exclusive locks on it
// through the two aliases, which is the failure loto exists to prevent.
func TestResolveCLITarget_SymlinkedAncestorConverges(t *testing.T) {
	top := evalTop(t, t.TempDir())
	realDir := filepath.Join(top, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, tcTargetA), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(top, "link")); err != nil {
		t.Fatal(err)
	}

	viaLink, err := resolveCLITarget(top, top, "link/"+tcTargetA)
	if err != nil {
		t.Fatalf("via link: %v", err)
	}
	viaReal, err := resolveCLITarget(top, top, "real/"+tcTargetA)
	if err != nil {
		t.Fatalf("via real: %v", err)
	}
	if viaLink.Canonical != viaReal.Canonical {
		t.Errorf("aliases must converge: link=%q real=%q", viaLink.Canonical, viaReal.Canonical)
	}
	if viaLink.Canonical != "real/"+tcTargetA {
		t.Errorf("canonical = %q, want real/%s", viaLink.Canonical, tcTargetA)
	}
}

// TestResolveCLITarget_DeepMissingLeafUnderSymlinkedAncestor pins the tail walk.
// A beacon announces a write to a path that does not exist yet; the old
// one-level fallback resolved only Dir(p), so two missing segments left the
// symlinked ancestor unresolved and the alias intact.
func TestResolveCLITarget_DeepMissingLeafUnderSymlinkedAncestor(t *testing.T) {
	top := evalTop(t, t.TempDir())
	realDir := filepath.Join(top, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(top, "link")); err != nil {
		t.Fatal(err)
	}

	got, err := resolveCLITarget(top, top, "link/pkg/brand-new.go")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Canonical != "real/pkg/brand-new.go" {
		t.Errorf("canonical = %q, want real/pkg/brand-new.go", got.Canonical)
	}
}

// TestResolveCLITarget_SymlinkedAncestorOutOfRepoEscapes closes the hole the
// lexical containment check left open: filepath.Rel could not see that an
// in-repo-looking prefix was a symlink pointing somewhere else entirely.
func TestResolveCLITarget_SymlinkedAncestorOutOfRepoEscapes(t *testing.T) {
	top := evalTop(t, t.TempDir())
	outside := evalTop(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(outside, tcTargetA), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(top, "escape")); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveCLITarget(top, top, "escape/"+tcTargetA); !errors.Is(err, domain.ErrRepoEscape) {
		t.Errorf("err = %v, want ErrRepoEscape", err)
	}
}

// TestResolveAncestors_MissingTailRidesAlong pins both ends of the walk: the
// existing prefix is resolved, and every segment that does not exist yet is
// re-appended verbatim.
func TestResolveAncestors_MissingTailRidesAlong(t *testing.T) {
	dir := t.TempDir()
	resolved := evalTop(t, dir)
	got := resolveAncestors(filepath.Join(dir, "nope", "deeper", "x.go"))
	if want := filepath.Join(resolved, "nope", "deeper", "x.go"); got != want {
		t.Errorf("resolveAncestors = %q, want %q", got, want)
	}
	// No existing prefix at all: nothing on disk to alias, so nothing changes.
	orphaned := filepath.Join(string(filepath.Separator), "no-such-root-9f2a", "x.go")
	if got := resolveAncestors(orphaned); got != orphaned {
		t.Errorf("resolveAncestors(%q) = %q, want it unchanged", orphaned, got)
	}
}
