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
	ErrRepoEscape        = errors.New("target resolves outside the repository")
	ErrEmptyTarget       = errors.New("empty target")
	ErrTargetHasNUL      = errors.New("target contains NUL")
	ErrTargetBackslash   = errors.New("target contains backslash; use POSIX separators")
	ErrTargetIsRepoRoot  = errors.New("target must not be the repo root")
	ErrTargetIsGlob      = errors.New("glob targets not supported; pass an explicit file path")
	ErrTargetIsDir       = errors.New("directory targets not supported; pass an explicit file path")
	ErrTargetUnspellable = errors.New("target cannot be a repo path; it looks like an unexpanded or unquoted shell token")
)

// shellMeta are the characters that make a token a shell construct rather than
// a path: an expansion ($VAR, `cmd`) or a quote the shell would have removed.
// No plausible repo path carries one, so a target that does is a token whose
// caller believed the shell had already rewritten it (loto-bl66: a live lock
// on the literal string "$FAKE_HOME", minted by the PreToolUse gate).
//
// ‡ A plain interior space is NOT here. The PreToolUse hook's own extraction
//
//	rule refuses spaces, but that is a conservative heuristic for splitting a
//	command line; `my file.go` is an awkward but legal filename, and refusing
//	it in the domain would regress `loto lock` on a file that really exists.
//	Leading and trailing whitespace IS refused below — that is never part of a
//	name a caller meant to type.
const shellMeta = "$`\"'"

// hasControl reports whether s carries an ASCII control character. A newline or
// tab in a target splits the one-row-per-line surfaces the PreToolUse hook and
// `loto status` are parsed from, so it can never be a legal target regardless
// of what a filesystem would accept.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

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
	// Spelling rule, decided before any filesystem question, so every verb
	// agrees on what a path even is — `lock` (which also checks existence),
	// `beacon` (which must NOT, since it announces a write to a file that does
	// not exist yet), `tag`, and `claim` through CanonicalizePrefix. Keeping it
	// here is what stops beacon and lock from drifting apart (loto-bl66).
	if strings.ContainsAny(in, shellMeta) || hasControl(in) ||
		strings.TrimSpace(in) != in {
		return Target{}, ErrTargetUnspellable
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
// (loto-7af9). Trailing slashes are the natural directory spelling, so all of
// them are trimmed — a single TrimSuffix would leave "a//" as "a/" and
// misreport the prefix with the file-verb ErrTargetIsDir. Everything else —
// NUL/backslash/escape/glob/repo-root rejection — delegates to Canonicalize
// so one policy source governs both surfaces. "./" trims to "." and stays
// rejected as repo-root; "/" is kept intact so it reports repo-escape.
func CanonicalizePrefix(in string) (Target, error) {
	if in != "" && in != "/" {
		in = strings.TrimRight(in, "/")
	}
	return Canonicalize(in)
}
