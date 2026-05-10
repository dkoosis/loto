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

func Canonicalize(in string) (Target, error) {
	if in == "" {
		return Target{}, errors.New("empty target")
	}
	if strings.ContainsRune(in, 0) {
		return Target{}, errors.New("target contains NUL")
	}
	if strings.ContainsRune(in, '\\') {
		return Target{}, errors.New("target contains backslash; use POSIX separators")
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
		return Target{}, errors.New("target must not be the repo root")
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
