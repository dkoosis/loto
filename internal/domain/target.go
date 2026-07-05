package domain

import (
	"errors"
	"path"
	"strings"
)

type Target struct {
	Canonical string
}

var (
	ErrRepoEscape       = errors.New("target resolves outside the repository")
	ErrEmptyTarget      = errors.New("empty target")
	ErrTargetHasNUL     = errors.New("target contains NUL")
	ErrTargetBackslash  = errors.New("target contains backslash; use POSIX separators")
	ErrTargetIsRepoRoot = errors.New("target must not be the repo root")
	ErrTargetIsGlob     = errors.New("glob targets not supported; pass an explicit file path")
	ErrTargetIsDir      = errors.New("directory targets not supported; pass an explicit file path")
)

func Canonicalize(in string) (Target, error) {
	if in == "" {
		return Target{}, ErrEmptyTarget
	}
	if strings.ContainsRune(in, 0) {
		return Target{}, ErrTargetHasNUL
	}
	if strings.ContainsRune(in, '\\') {
		return Target{}, ErrTargetBackslash
	}
	if strings.HasPrefix(in, "/") {
		return Target{}, ErrRepoEscape
	}
	if strings.ContainsAny(in, "*?[{") {
		return Target{}, ErrTargetIsGlob
	}
	if strings.HasSuffix(in, "/") {
		return Target{}, ErrTargetIsDir
	}
	cleaned := path.Clean(in)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return Target{}, ErrRepoEscape
	}
	if cleaned == "." {
		return Target{}, ErrTargetIsRepoRoot
	}
	return Target{Canonical: cleaned}, nil
}

// CanonicalizePrefix canonicalizes a directory-prefix claim target
// (loto-7af9). A trailing slash is the natural directory spelling, so it is
// trimmed rather than rejected; everything else — NUL/backslash/escape/glob/
// repo-root rejection — delegates to Canonicalize so one policy source
// governs both surfaces. "./" trims to "." and stays rejected as repo-root.
func CanonicalizePrefix(in string) (Target, error) {
	if in != "" && in != "/" {
		in = strings.TrimSuffix(in, "/")
	}
	return Canonicalize(in)
}
