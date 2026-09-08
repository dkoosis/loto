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

// HasControl reports whether s carries an ASCII control character. A newline or
// tab in a target splits the one-row-per-line surfaces the PreToolUse hook and
// `loto status` are parsed from, so a CALLER-TYPED target carrying one is
// refused below. Exported because a git-provenance path is admitted with one
// (see Provenance) and the surfaces that print it have to escape it rather
// than emit a row that splits.
func HasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Provenance says who produced a path token. It selects exactly one rule —
// the shell-token rule below — and nothing else: containment, case folding,
// NUL, backslash, glob and directory spelling apply to every provenance.
//
// ‡ The shell-token rule (shellMeta / HasControl / surrounding whitespace)
// exists to refuse an unexpanded token a CALLER typed, in the belief the
// shell had already rewritten it (loto-bl66). A path git printed —
// `git diff --cached -z` — never passed through a shell, so a `"` or a tab in
// it is a filename, not a token nobody expanded. Applying the rule there made
// the staged-lock gate refuse to canonicalize an ordinary git state, and the
// gate's caller read that refusal as "nothing to check" (loto-pgio).
type Provenance int

const (
	// ProvenanceTyped is a token a caller typed: a CLI positional, a hook's
	// extraction from a command line. The default, and the strict one.
	ProvenanceTyped Provenance = iota
	// ProvenanceGit is a token git printed with NUL framing. Every rule
	// applies except the shell-token one.
	ProvenanceGit
)

// Canonicalize applies the full spelling and containment policy to a
// caller-typed token. It is CanonicalizeFrom(in, ProvenanceTyped).
func Canonicalize(in string) (Target, error) {
	return CanonicalizeFrom(in, ProvenanceTyped)
}

// CanonicalizeFrom is Canonicalize with the token's provenance named. See
// Provenance for the single rule the choice moves.
func CanonicalizeFrom(in string, prov Provenance) (Target, error) {
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
	if prov == ProvenanceTyped &&
		(strings.ContainsAny(in, shellMeta) || HasControl(in) ||
			strings.TrimSpace(in) != in) {
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
// NUL/backslash/escape/glob rejection — delegates to Canonicalize so one
// policy source governs both surfaces. "/" is kept intact so it reports
// repo-escape.
//
// The one rule that does NOT carry over is the repo-root rejection (sd-isv2):
// "." and "./" are a legal claim prefix. The root is simply the widest
// territory, and a takeover of a shared checkout reserves exactly that. `lock`
// still refuses it, because a lock names a write-set and "every file" is not
// one — so this is the single point where the two verbs' spelling rules
// diverge, which is why the divergence lives here rather than as a flag
// threaded through Canonicalize.
func CanonicalizePrefix(in string) (Target, error) {
	if in != "" && in != "/" {
		in = strings.TrimRight(in, "/")
	}
	t, err := Canonicalize(in)
	// ‡ Rescuing the ERROR, not short-circuiting on the input, is load-bearing.
	// An early `if path.Clean(in) == "."` would also accept `$X/..`, whose
	// Clean is "." — bypassing the shellMeta check that exists to refuse an
	// unexpanded shell token (loto-bl66). Canonicalize runs every other rule
	// first and reports ErrTargetUnspellable for that one, so only a spelling
	// whose sole fault is being the root ever reaches the arm below.
	if errors.Is(err, ErrTargetIsRepoRoot) {
		return Target{Canonical: "."}, nil
	}
	return t, err
}
