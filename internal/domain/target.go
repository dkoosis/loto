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
