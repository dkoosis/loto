package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"loto/internal/domain"
)

const unnamedSlug = "unnamed"

// StateDir returns the state directory for the project rooted at repoTop:
// $XDG_STATE_HOME/loto/projects/<slug>/. LOTO_BASE overrides everything.
func StateDir(repoTop string) string {
	if v := os.Getenv("LOTO_BASE"); v != "" {
		return v
	}
	return filepath.Join(xdgStateHome(), "loto", "projects", ResolveAndPinProjectSlug(repoTop))
}

func xdgStateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "state")
	}
	return filepath.Join(home, ".local", "state")
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
	dir := filepath.Dir(pinFile)
	tmp, err := os.CreateTemp(dir, ".loto-slug.*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(slug + "\n"); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return
	}
	// Sync the bytes before rename, then fsync the parent dir after — without
	// both, the pin's directory entry can be lost on power loss (loto-cq6 /
	// gh#131). Best-effort throughout: the caller uses the slug it computed
	// regardless, so a flush failure must not abort.
	_ = tmp.Sync()
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Rename(tmpName, pinFile); err != nil {
		return
	}
	_ = syncDir(dir)
}

// syncDir flushes a directory's metadata so a rename inside it survives power
// loss. Call after the file's own bytes are fsync'd. (Duplicated from
// internal/identity rather than shared: that package must import no internal
// package — see .go-arch-lint.yml — and the helper is small enough to stay
// under jscpd limits.)
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
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

// resolveTargets canonicalizes arg into one or more Target values.
// Currently returns a single target; glob expansion at call time is bead loto-1wl.
func resolveTargets(arg string) ([]domain.Target, error) {
	t, err := domain.Canonicalize(arg)
	if err != nil {
		return nil, err
	}
	return []domain.Target{t}, nil
}

// resolveCLITarget normalizes a user-supplied path (absolute, relative, or
// inside repoTop) into a canonical domain.Target. Centralizes the
// normalizeRepoPath + Canonicalize policy so future fixes land in one place.
func resolveCLITarget(repoTop, raw string) (domain.Target, error) {
	return domain.Canonicalize(normalizeRepoPath(raw, repoTop))
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
	absP := filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(absP); err == nil {
		absP = r
	} else if r, err := filepath.EvalSymlinks(filepath.Dir(absP)); err == nil {
		// p doesn't exist yet (e.g., loto check on a newly added file under
		// `git diff --cached`). Resolve symlinks on the longest existing prefix.
		absP = filepath.Join(r, filepath.Base(absP))
	}
	rel, err := filepath.Rel(absTop, absP)
	if err != nil {
		return p
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.ToSlash(rel)
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
