package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/dkoosis/atomicfile"

	"loto/internal/domain"
)

const unnamedSlug = "unnamed"

// StateDir returns the state directory for the project rooted at repoTop:
// $XDG_STATE_HOME/loto/projects/<slug>/. LOTO_BASE overrides everything.
func StateDir(repoTop string) string {
	// LOTO_BASE wins outright, and it is the supported way to give a second
	// CLONE of one repo its own store: the slug below is derived from the
	// origin remote, so two clones otherwise land on one DB with nothing
	// recorded to tell their checkouts apart (loto-n7xb, reasoned out on
	// store.Store.repoTop).
	if v := os.Getenv("LOTO_BASE"); v != "" {
		return v
	}
	return filepath.Join(xdgStateHome(), "loto", "projects", ResolveAndPinProjectSlug(repoTop))
}

// xdgStateHome mirrors identity.homeDir's cascade (registry.go): prefer
// os.UserHomeDir ($HOME), fall back to os/user.Current().HomeDir (getpwuid_r)
// when $HOME is unset, and only then /tmp. Duplicated rather than shared —
// identity must import no internal package (.go-arch-lint.yml) — but this
// keeps the fallback ABSOLUTE. The old bare os.UserHomeDir()-fails case
// returned a relative ".local/state", silently rooting the state dir at
// whatever cwd the command happened to run from.
func xdgStateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	home := fallbackHomeDir()
	return filepath.Join(home, ".local", "state")
}

// fallbackHomeDir resolves a home directory that's always absolute: prefer
// os.UserHomeDir ($HOME), fall back to os/user.Current().HomeDir
// (getpwuid_r) when $HOME is unset, and only then /tmp.
func fallbackHomeDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return "/tmp"
}

// ResolveAndPinProjectSlug returns a stable slug for the repo at repoTop. Uses pinned slug
// in $GIT_COMMON_DIR/.loto-slug if present; else origin remote; else dir name.
func ResolveAndPinProjectSlug(repoTop string) string {
	if slug := pinnedSlug(repoTop); slug != "" {
		return slug
	}
	if slug := slugFromRemote(repoTop); slug != "" {
		pinSlug(repoTop, slug)
		return slug
	}
	slug := slugFromDir(repoTop)
	pinSlug(repoTop, slug)
	return slug
}

func pinnedSlug(repoTop string) string {
	pinFile := gitCommonDirFile(repoTop, ".loto-slug")
	if pinFile == "" {
		return ""
	}
	data, err := os.ReadFile(pinFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// pinSlug atomically writes the pinned slug. Worktrees sharing GIT_COMMON_DIR
// could otherwise observe a torn read or a clobbered partial file during the
// pre-pin window (audit loto-7c0). Errors are silenced here because the caller
// uses the slug it just computed regardless — but the temp+rename guarantees
// readers never see a half-written file.
func pinSlug(repoTop, slug string) {
	pinFile := gitCommonDirFile(repoTop, ".loto-slug")
	if pinFile == "" {
		return
	}
	// Sync the bytes before rename, then fsync the parent dir after — without
	// both, the pin's directory entry can be lost on power loss (loto-cq6 /
	// gh#131). atomicfile owns that sequence and adds F_FULLFSYNC on darwin.
	// Best-effort: the caller uses the slug it computed regardless, so a write
	// failure must not abort.
	_ = atomicfile.WriteFile(pinFile, []byte(slug+"\n"), 0o600)
}

func gitCommonDirFile(repoTop, name string) string {
	out, err := gitCmd(context.Background(), repoTop, "rev-parse", "--git-common-dir")
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(out)
	if dir == "" {
		return ""
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoTop, dir)
	}
	return filepath.Join(dir, name)
}

func slugFromRemote(repoTop string) string {
	out, err := gitCmd(context.Background(), repoTop, "remote", "get-url", "origin")
	if err != nil {
		remotes, err2 := gitCmd(context.Background(), repoTop, "remote")
		if err2 != nil || strings.TrimSpace(remotes) == "" {
			return ""
		}
		first := strings.Fields(strings.TrimSpace(remotes))[0]
		if first != "origin" {
			fmt.Fprintf(os.Stderr, "loto: warning: no 'origin' remote; using %q for project slug\n", first)
		}
		out, err = gitCmd(context.Background(), repoTop, "remote", "get-url", first)
		if err != nil {
			return ""
		}
	}
	return normalizeURL(strings.TrimSpace(out))
}

func normalizeURL(rawURL string) string {
	s := rawURL
	for _, pfx := range []string{"https://", "http://", "git://", "ssh://"} {
		s = strings.TrimPrefix(s, pfx)
	}
	// Strip host component: SSH-shorthand "user@host:owner/repo" via colon, or
	// "host/owner/repo" via first slash. Do exactly one strip.
	if i := strings.Index(s, ":"); i != -1 && !strings.Contains(s[:i], "/") {
		s = s[i+1:]
	} else if i := strings.Index(s, "/"); i != -1 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".git")
	s = strings.NewReplacer("/", "-", "_", "-", ".", "-").Replace(s)
	if s == "" {
		return unnamedSlug
	}
	return s
}

func slugFromDir(repoTop string) string {
	if out, err := gitCmd(context.Background(), repoTop, "rev-parse", "--show-toplevel"); err == nil {
		if base := filepath.Base(strings.TrimSpace(out)); base != "" && base != "." {
			return base
		}
	}
	if base := filepath.Base(repoTop); base != "" && base != "." {
		return base
	}
	return unnamedSlug
}

// gitCmd runs git under gitTimeout on top of the caller-supplied ctx so SIGINT
// propagates into the git subprocess (audit loto-p7j) and a hung repo (stale
// NFS, fsmonitor wedge) still completes. Boot-path callers can pass
// context.Background() — the timeout alone is sufficient there.
func gitCmd(ctx context.Context, repoTop string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if repoTop != "" {
		cmd.Dir = repoTop
	}
	out, err := cmd.Output()
	return string(out), err
}

// errCallerCWDUnknown reports that a relative token cannot be resolved because
// the caller's base directory is not knowable (os.Getwd failed — a deleted cwd).
// Refusing is the only safe answer: falling back to repo-root-relative is the
// false clean invariant 9 forbids.
var errCallerCWDUnknown = errors.New("caller cwd unknown: cannot resolve a relative path")

// callerBase returns the directory a caller-typed relative token resolves
// against — the process's own cwd, since loto was spawned by the shell the
// caller is standing in. Empty on failure, which resolveCLITarget turns into a
// refusal rather than a guess.
//
// ‡ os.Getwd (physical), deliberately not $PWD (logical). repoTop comes from
// `git rev-parse --show-toplevel` run against the same getcwd(2), so base and
// top agree by construction; $PWD is a mutable env var an agent's shell can
// leave stale, and a lock key is durable state.
func callerBase() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// resolveCLITarget normalizes a user-supplied path (absolute, relative, or
// inside repoTop) into a canonical domain.Target. Centralizes the
// normalizeRepoPath + Canonicalize policy so future fixes land in one place.
//
// base is the directory a RELATIVE token resolves against — the token's
// provenance, which only the caller knows (loto-3tv3):
//
//   - caller-typed positionals → the caller's cwd (callerBase())
//   - tokens git produced with cmd.Dir=repoTop (`check --staged`) → repoTop
//   - "" → the base is not knowable; relative tokens are refused
//
// The caller declares; loto must not sniff. Making the base a parameter is what
// keeps `check --staged` (git-produced, already repo-root-relative) from being
// re-based onto the cwd.
func resolveCLITarget(base, repoTop, raw string) (domain.Target, error) {
	if repoTop == "" || filepath.IsAbs(raw) {
		// No repo frame, or a token that carries its own base: today's path.
		return domain.Canonicalize(normalizeRepoPath(raw, repoTop))
	}
	// Spelling verdicts are base-independent and are decided on the RAW token;
	// positional verdicts are base-dependent and are re-decided after the join.
	// filepath.Join Cleans, which erases the spellings Canonicalize rules on
	// (a trailing slash, a glob metacharacter), so they must be judged first.
	if _, err := domain.Canonicalize(raw); err != nil {
		switch {
		case errors.Is(err, domain.ErrTargetIsRepoRoot), errors.Is(err, domain.ErrRepoEscape):
			// positional: base-dependent, re-decided below
		default:
			return domain.Target{}, err
		}
	}
	if base == "" {
		return domain.Target{}, errCallerCWDUnknown
	}
	rel, err := repoRelFromBase(base, repoTop, raw)
	if err != nil {
		return domain.Target{}, err
	}
	return domain.Canonicalize(rel)
}

// repoRelFromBase joins a relative token to its base and expresses the result
// repo-relative, or reports domain.ErrRepoEscape when it lands outside repoTop.
//
// ‡ Symlinked ANCESTORS are resolved (resolveAncestors); the final component is
// not, so a symlink target is still refused downstream. Resolving before the
// containment check also closes a hole: a symlinked directory pointing outside
// the repo used to pass containment, because filepath.Rel is lexical.
func repoRelFromBase(base, repoTop, raw string) (string, error) {
	absTop, err := filepath.Abs(repoTop)
	if err != nil {
		return "", err
	}
	// Same reason as normalizeRepoPath: /var vs /private/var on macOS, and any
	// other symlinked checkout root.
	if r, err := filepath.EvalSymlinks(absTop); err == nil {
		absTop = r
	}
	// Resolve symlinks on the BASE — a directory the caller is standing in, not
	// the token — so a logical cwd (/var/... on macOS, where os.Getwd can answer
	// from $PWD) lines up with the physical repoTop. Without this every token
	// from a temp-dir checkout reads as a repo escape.
	absBase := base
	if r, err := filepath.EvalSymlinks(absBase); err == nil {
		absBase = r
	}
	absP := resolveAncestors(filepath.Join(absBase, raw))
	rel, err := filepath.Rel(absTop, absP)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return ".", nil // Canonicalize answers ErrTargetIsRepoRoot
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if sub, ok := foldContains(absTop, absP); ok && caseInsensitiveFS(absTop) {
			return filepath.ToSlash(sub), nil
		}
		return "", domain.ErrRepoEscape
	}
	return filepath.ToSlash(rel), nil
}

// normalizeRepoPath translates an absolute path that lies inside repoTop to a
// repo-relative POSIX path so domain.Canonicalize (which rejects absolute
// paths) accepts it. Inputs that are already relative, lie outside repoTop, or
// fail filepath ops are returned unchanged so the caller still sees the
// original token in error output.
//
// Fix for loto-d3l: `loto check /abs/path` used to silently report "no
// conflicts" for files locked under the equivalent relative form, because the
// CLI swallowed the ErrRepoEscape from Canonicalize.
func normalizeRepoPath(p, repoTop string) string {
	if p == "" || repoTop == "" || !filepath.IsAbs(p) {
		return p
	}
	absTop, err := filepath.Abs(repoTop)
	if err != nil {
		return p
	}
	// EvalSymlinks the top so /var/... vs /private/var/... (macOS tmp) and
	// other symlinked checkout roots resolve. We deliberately don't resolve p:
	// symlink-as-lock-target is rejected upstream (Lstat), and skipping
	// EvalSymlinks(p) avoids a failure mode when p doesn't exist on disk yet
	// (e.g., loto check on a newly added file).
	if r, err := filepath.EvalSymlinks(absTop); err == nil {
		absTop = r
	}
	absP := resolveAncestors(p)
	rel, err := filepath.Rel(absTop, absP)
	if err != nil {
		return p
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// filepath.Rel is lexical and case-sensitive. On a case-insensitive
		// filesystem (macOS APFS default, Windows) the segments at/above the
		// checkout root can differ in case from git's recorded path: a worktree
		// minted from a lowercase cwd yields /Users/x/projects/... while git
		// reports /Users/x/Projects/.... Rel then reports a bogus escape and the
		// file is wrongly rejected as repo-escape (loto-d3l, case variant).
		// Retry containment case-insensitively, but only when the filesystem
		// actually folds case, so case-sensitive systems keep exact semantics.
		if sub, ok := foldContains(absTop, absP); ok && caseInsensitiveFS(absTop) {
			return filepath.ToSlash(sub)
		}
		return p
	}
	return filepath.ToSlash(rel)
}

// resolveAncestors resolves symlinks on an absolute path's DIRECTORY prefix and
// re-appends the final component untouched.
//
// ‡ Two decisions live here, and they pull in opposite directions.
//
// Ancestors are resolved because domain.Canonicalize is purely lexical: with
// `link` a symlinked directory, `link/a.go` and `real/a.go` produced different
// keys for one file, so two agents could hold exclusive locks on it through two
// aliases — the exact failure loto exists to prevent (loto-j39r defect 1).
// Lstat only ever refused a symlinked FINAL component; intermediate symlinks
// were followed silently.
//
// The final component is NOT resolved, so a token naming a symlink still
// reaches statFileTargetReason and is still refused. Resolving it would make
// `loto lock sym.go` quietly lock a.go instead — and refusal already denies the
// alias any way to double-hold, which is what convergence was for. Absolute
// spellings resolved the leaf before this change and now do not; that is the
// asymmetry loto-j39r named, unified toward the stricter half.
//
// ‡ The missing tail can be more than one segment. A beacon announces a write
// to a path that does not exist yet — `loto beacon a/b/c.go` with `a` a symlink
// and `b` not yet created — and the previous one-level fallback (EvalSymlinks
// on Dir(p) only) gave up there, leaving the symlinked ancestor unresolved and
// the alias intact.
func resolveAncestors(abs string) string {
	dir, leaf := filepath.Split(filepath.Clean(abs))
	if leaf == "" {
		return filepath.Clean(abs)
	}
	return filepath.Join(resolveExistingDir(filepath.Clean(dir)), leaf)
}

// resolveExistingDir resolves symlinks on the longest existing prefix of dir and
// re-appends the segments that do not exist yet. A path with no existing prefix
// comes back unchanged — there is nothing on disk to alias.
func resolveExistingDir(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	var tail []string
	cur := dir
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir // reached the root without resolving anything
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		if r, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(append([]string{r}, tail...)...)
		}
		cur = parent
	}
}

// foldContains reports whether child lies within parent using a case-insensitive
// boundary comparison, returning the in-repo remainder. Inputs must be cleaned
// absolute paths. Only the prefix up to parent's length is compared by fold; the
// returned remainder preserves child's casing (below the checkout root both
// sessions reference the same on-disk names, so that case is authoritative).
func foldContains(parent, child string) (string, bool) {
	if len(child) <= len(parent) {
		return "", false
	}
	if child[len(parent)] != filepath.Separator {
		return "", false // parent is a string prefix, not a path-component prefix
	}
	if !strings.EqualFold(child[:len(parent)], parent) {
		return "", false
	}
	return child[len(parent)+1:], true
}

// caseInsensitiveFS reports whether dir resides on a case-insensitive filesystem
// by checking that a case-flipped variant of dir resolves to the same inode. dir
// must exist. Returns false (the conservative, case-sensitive assumption) on any
// error or when dir's basename has no ASCII letter to flip.
func caseInsensitiveFS(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	flipped := flipBasenameCase(dir)
	if flipped == dir {
		return false
	}
	ffi, err := os.Stat(flipped)
	if err != nil {
		return false
	}
	return os.SameFile(fi, ffi)
}

// flipBasenameCase returns dir with the case of the first ASCII letter in its
// basename inverted, or dir unchanged if the basename has no ASCII letter.
func flipBasenameCase(dir string) string {
	d, base := filepath.Split(dir)
	b := []byte(base)
	for i := range b {
		switch {
		case b[i] >= 'a' && b[i] <= 'z':
			b[i] -= 32
			return d + string(b)
		case b[i] >= 'A' && b[i] <= 'Z':
			b[i] += 32
			return d + string(b)
		}
	}
	return dir
}

// relPath returns p relative to the current working directory when both lie
// on the same volume and the result doesn't escape cwd with "../" prefixes
// (which would be longer than the absolute path). Falls back to p on any
// error. Per .claude/rules/design.md — prefer relative paths in output.
func relPath(p string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(cwd, p)
	if err != nil {
		return p
	}
	if strings.HasPrefix(rel, "..") {
		return p
	}
	return rel
}
