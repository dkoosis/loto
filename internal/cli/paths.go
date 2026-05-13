package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"loto/internal/domain"
)

const unnamedSlug = "unnamed"

// StateDir returns the state directory for the project rooted at repoTop:
// $XDG_STATE_HOME/loto/projects/<slug>/. LOTO_BASE overrides everything.
func StateDir(repoTop string) string {
	if v := os.Getenv("LOTO_BASE"); v != "" {
		return v
	}
	return filepath.Join(xdgStateHome(), "loto", "projects", ProjectSlug(repoTop))
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

// ProjectSlug returns a stable slug for the repo at repoTop. Uses pinned slug
// in $GIT_COMMON_DIR/.loto-slug if present; else origin remote; else dir name.
func ProjectSlug(repoTop string) string {
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
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmpName, pinFile)
}

func gitCommonDirFile(repoTop, name string) string {
	out, err := gitCmd(repoTop, "rev-parse", "--git-common-dir")
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
	out, err := gitCmd(repoTop, "remote", "get-url", "origin")
	if err != nil {
		remotes, err2 := gitCmd(repoTop, "remote")
		if err2 != nil || strings.TrimSpace(remotes) == "" {
			return ""
		}
		first := strings.Fields(strings.TrimSpace(remotes))[0]
		if first != "origin" {
			fmt.Fprintf(os.Stderr, "loto: warning: no 'origin' remote; using %q for project slug\n", first)
		}
		out, err = gitCmd(repoTop, "remote", "get-url", first)
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
	if out, err := gitCmd(repoTop, "rev-parse", "--show-toplevel"); err == nil {
		if base := filepath.Base(strings.TrimSpace(out)); base != "" && base != "." {
			return base
		}
	}
	if base := filepath.Base(repoTop); base != "" && base != "." {
		return base
	}
	return unnamedSlug
}

func gitCmd(repoTop string, args ...string) (string, error) {
	// Derive from runtimeCtx so SIGINT propagates into the git subprocess
	// (audit loto-p7j). Boot-path callers — ProjectSlug, gitCommonDirFile —
	// still get a 10s upper bound on top of any ambient cancellation.
	ctx, cancel := context.WithTimeout(runtimeCtx(), 10*time.Second)
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
