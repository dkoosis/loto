package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// defaultBase returns the canonical coordination directory for the current
// project. See docs/decisions/0002-canonical-base.md.
func defaultBase() string {
	if v := os.Getenv("LOTO_BASE"); v != "" {
		return v
	}
	return filepath.Join(xdgStateHome(), "loto", "projects", projectSlug())
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

func projectSlug() string {
	if slug := pinnedSlug(); slug != "" {
		return slug
	}
	warnLegacy()
	if slug := slugFromRemote(); slug != "" {
		pinSlug(slug)
		return slug
	}
	slug := slugFromDir()
	pinSlug(slug)
	return slug
}

func pinnedSlug() string {
	pinFile := gitCommonDirFile(".loto-slug")
	if pinFile == "" {
		return ""
	}
	data, err := os.ReadFile(pinFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func pinSlug(slug string) {
	pinFile := gitCommonDirFile(".loto-slug")
	if pinFile == "" {
		return
	}
	_ = os.WriteFile(pinFile, []byte(slug+"\n"), 0o600)
}

func gitCommonDirFile(name string) string {
	out, err := gitCmd("rev-parse", "--git-common-dir")
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(out)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

func slugFromRemote() string {
	out, err := gitCmd("remote", "get-url", "origin")
	if err != nil {
		remotes, err2 := gitCmd("remote")
		if err2 != nil || strings.TrimSpace(remotes) == "" {
			return ""
		}
		first := strings.Fields(strings.TrimSpace(remotes))[0]
		if first != "origin" {
			fmt.Fprintf(os.Stderr, "loto: warning: no 'origin' remote; using %q for project slug\n", first)
		}
		out, err = gitCmd("remote", "get-url", first)
		if err != nil {
			return ""
		}
	}
	return normalizeURL(strings.TrimSpace(out))
}

// normalizeURL converts a remote URL to a slug.
// "git@github.com:dkoosis/loto.git" → "dkoosis-loto"
// "https://github.com/dkoosis/loto"  → "dkoosis-loto"
func normalizeURL(rawURL string) string {
	s := rawURL
	for _, pfx := range []string{"https://", "http://", "git://", "ssh://"} {
		s = strings.TrimPrefix(s, pfx)
	}
	// Strip user@host: (SSH shorthand).
	if i := strings.Index(s, ":"); i != -1 && !strings.Contains(s[:i], "/") {
		s = s[i+1:]
	}
	// Strip host component.
	if i := strings.Index(s, "/"); i != -1 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".git")
	s = strings.NewReplacer("/", "-", "_", "-", ".", "-").Replace(s)
	if s == "" {
		return "unnamed"
	}
	return s
}

func slugFromDir() string {
	if out, err := gitCmd("rev-parse", "--show-toplevel"); err == nil {
		if base := filepath.Base(strings.TrimSpace(out)); base != "" && base != "." {
			return base
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "unnamed"
	}
	if base := filepath.Base(cwd); base != "" && base != "." {
		return base
	}
	return "unnamed"
}

func gitCmd(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return string(out), err
}

var legacyWarned bool

func warnLegacy() {
	if legacyWarned || os.Getenv("LOTO_SUPPRESS_LEGACY_WARNING") == "1" {
		return
	}
	cwd, _ := os.Getwd()
	if _, err := os.Stat(filepath.Join(cwd, ".loto")); err == nil {
		fmt.Fprintln(os.Stderr, "loto: warning: found legacy ./.loto directory — coordination now uses $XDG_STATE_HOME/loto/projects/<slug>/. The old directory is not read.")
		legacyWarned = true
	}
}
