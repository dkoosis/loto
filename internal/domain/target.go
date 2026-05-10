package domain

import (
	"errors"
	"path"
	"strings"
)

type TargetKind int

const (
	KindFile TargetKind = iota
	KindDir
	KindGlob
)

type Target struct {
	Canonical string
	Kind      TargetKind
}

var ErrRepoEscape = errors.New("target resolves outside the repository")

var (
	ErrEmptyTarget      = errors.New("empty target")
	ErrTargetHasNUL     = errors.New("target contains NUL")
	ErrTargetBackslash  = errors.New("target contains backslash; use POSIX separators")
	ErrTargetIsRepoRoot = errors.New("target must not be the repo root")
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
	hasGlob := strings.ContainsAny(in, "*?[{")
	hadTrailingSlash := strings.HasSuffix(in, "/")
	cleaned := path.Clean(in)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return Target{}, ErrRepoEscape
	}
	if cleaned == "." {
		return Target{}, ErrTargetIsRepoRoot
	}
	switch {
	case hasGlob:
		return Target{Canonical: cleaned, Kind: KindGlob}, nil
	case hadTrailingSlash:
		return Target{Canonical: cleaned + "/", Kind: KindDir}, nil
	default:
		return Target{Canonical: cleaned, Kind: KindFile}, nil
	}
}
