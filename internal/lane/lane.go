// Package lane builds per-lane git branch refs by pure plumbing — no checkout,
// no HEAD move, no shared-index mutation — so N parallel lanes can commit
// disjoint write-sets out of one shared working tree.
//
// It is the engine half of loto-9sro. Lock coordination (which lane is allowed
// to write which paths) lives in the CLI layer; this package assumes the caller
// already holds those locks. lane_test.go is the executable spec — it ports the
// isolation assertions the loto-9sro plumbing/daemon-dirty spikes proved.
package lane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitTimeout caps each plumbing git call. The operations here (read-tree,
// write-tree, commit-tree, update-ref) are fast; the bound only guards against a
// wedged repo (stale NFS, fsmonitor) turning a lane commit into a hung process.
const gitTimeout = 30 * time.Second

const gitRevParse = "rev-parse"

// Validation sentinels. Each Opts/Ref/WriteSet rejection wraps one of these so
// callers can errors.Is against a stable target (repo convention: static errors,
// not ad-hoc strings).
var (
	errMissingField    = errors.New("lane: missing required field")
	errInvalidRef      = errors.New("lane: invalid Ref")
	errInvalidWriteSet = errors.New("lane: invalid WriteSet")
	errBadBase         = errors.New("lane: bad Base")
	errIndexPath       = errors.New("lane: empty index path")
)

// Identity is a git author or committer principal. commit-tree bypasses git's
// own identity resolution, so both the author and committer must be supplied
// explicitly — there is no git config fallback.
type Identity struct {
	Name  string
	Email string
}

// Opts parameterizes Commit. RepoTop, Base, Ref, WriteSet, Message, Author and
// Committer are required.
type Opts struct {
	// RepoTop is the working-tree root; every git command runs there.
	RepoTop string
	// Base is the commit-ish each lane forks from: the first lane commit's parent
	// and the index seed whenever the lane has no prior tip.
	Base string
	// Ref is the short lane name. The branch written is refs/heads/loto/<Ref>.
	Ref string
	// WriteSet is the exact set of repo-relative (slash-separated) paths to stage.
	// Paths are staged NUL-delimited; a directory or glob is never expanded —
	// every file is listed explicitly. Staging anything wider would sweep peers'
	// dirty edits into this lane (the fs84 bug this package exists to avoid).
	WriteSet []string
	// Message is the full commit message, including the repo's Closes: trailer.
	// commit-tree runs no commit-msg hook and adds no trailer.
	Message string
	// Author and Committer are written verbatim; commit-tree ignores git config.
	Author    Identity
	Committer Identity
}

// Commit builds refs/heads/loto/<Ref> from the lane's parent plus the exact
// WriteSet, using git plumbing only, and returns the new commit SHA.
//
// Mechanism: a per-lane index file (GIT_INDEX_FILE) is seeded from the lane's
// parent — the existing lane tip if refs/heads/loto/<Ref> resolves, else Base —
// the WriteSet is staged NUL-delimited, a tree is written, commit-tree records
// it with the supplied identities and message, and update-ref moves the lane
// branch. HEAD, the working tree, and the shared index are never touched.
//
// Precondition: the caller holds a loto write lock on every WriteSet path and
// holds it through this call. The window between the caller finishing its edits
// and Commit staging them is a TOCTOU surface this package cannot close alone.
func Commit(ctx context.Context, opts Opts) (string, error) {
	if err := opts.validate(); err != nil {
		return "", err
	}
	g := gitRunner{repoTop: opts.RepoTop}

	parent, err := g.resolveParent(ctx, opts.Ref, opts.Base)
	if err != nil {
		return "", err
	}

	tree, err := g.buildLaneTree(ctx, opts.Ref, parent, opts.WriteSet)
	if err != nil {
		return "", err
	}

	commit, err := g.run(ctx, gitCall{
		env:   opts.identityEnv(),
		stdin: opts.Message,
		args:  []string{"commit-tree", tree, "-p", parent},
	})
	if err != nil {
		return "", fmt.Errorf("lane: commit-tree: %w", err)
	}
	commit = strings.TrimSpace(commit)

	if _, err := g.run(ctx, gitCall{args: []string{"update-ref", "refs/heads/loto/" + opts.Ref, commit}}); err != nil {
		return "", fmt.Errorf("lane: update-ref: %w", err)
	}
	return commit, nil
}

// buildLaneTree seeds a fresh per-lane index from parent, stages exactly
// writeSet, and returns the written tree SHA. The index is seeded (not started
// empty) on purpose: an empty index yields a tree of only the touched paths, so
// the commit would delete every file the lane did not write.
func (g gitRunner) buildLaneTree(ctx context.Context, ref, parent string, writeSet []string) (string, error) {
	idx, err := g.laneIndexPath(ctx, ref)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(idx), 0o700); err != nil {
		return "", fmt.Errorf("lane: mkdir index dir: %w", err)
	}
	// Always start from a clean index; we re-seed from parent every call.
	if err := os.Remove(idx); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("lane: clear stale index: %w", err)
	}
	idxEnv := []string{"GIT_INDEX_FILE=" + idx}

	if _, err := g.run(ctx, gitCall{env: idxEnv, args: []string{"read-tree", parent}}); err != nil {
		return "", fmt.Errorf("lane: seed index from %s: %w", parent, err)
	}

	if err := g.rejectTrackedDirWriteSet(ctx, idxEnv, writeSet); err != nil {
		return "", err
	}

	// Stage EXACTLY the write-set, NUL-delimited, each wrapped in :(literal) so a
	// wildcard or magic prefix that slipped past validateWriteSet is treated as a
	// literal path, never a pattern. -A captures additions and deletions for the
	// listed paths. NOTE: :(literal) does NOT stop a directory pathspec from
	// prefix-expanding — validateWriteSet rejects on-disk directories and
	// rejectTrackedDirWriteSet rejects any path tracked as a directory in the
	// parent (removed or file-shadowed), so none reach here.
	literal := make([]string, len(writeSet))
	for i, p := range writeSet {
		literal[i] = ":(literal)" + p
	}
	if _, err := g.run(ctx, gitCall{
		env:   idxEnv,
		stdin: strings.Join(literal, "\x00") + "\x00",
		args:  []string{"add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"},
	}); err != nil {
		return "", fmt.Errorf("lane: stage write-set: %w", err)
	}

	tree, err := g.run(ctx, gitCall{env: idxEnv, args: []string{"write-tree"}})
	if err != nil {
		return "", fmt.Errorf("lane: write-tree: %w", err)
	}
	return strings.TrimSpace(tree), nil
}

// rejectTrackedDirWriteSet closes the index/HEAD half of the directory-sweep
// vector that validateWriteSet's os.Stat cannot see. A write-set entry naming a
// directory that is tracked in the PARENT lets :(literal)<path> prefix-expand
// against the parent-seeded index in buildLaneTree's `git add -A`, staging a
// deletion for every file under <path>/ — the fs84 sweep (the symmetric
// index/HEAD case codex caught on #201). Two on-disk shapes reach here past
// validateWriteSet (which only rejects on-disk directories):
//   - removed entirely ('rm -rf pkg') — stats ENOENT;
//   - replaced by a regular file ('rm -rf pkg; echo x > pkg') — stats as a file.
//
// Both bypass the worktree check yet still sweep pkg/* deletions, so the probe
// must run regardless of on-disk presence. Probe each path against the
// already-seeded lane index (idxEnv): `git ls-files :(literal)<path>/` — the
// trailing slash forces a directory-prefix match, so a tracked FILE (a legitimate
// edit or deletion) yields zero rows and is allowed, while a path tracked as a
// directory in the parent yields its files and is rejected before any staging.
func (g gitRunner) rejectTrackedDirWriteSet(ctx context.Context, idxEnv, writeSet []string) error {
	for _, p := range writeSet {
		out, err := g.run(ctx, gitCall{env: idxEnv, args: []string{"ls-files", "--", ":(literal)" + p + "/"}})
		if err != nil {
			return fmt.Errorf("lane: probe path %q: %w", p, err)
		}
		if strings.TrimSpace(out) != "" {
			return fmt.Errorf("%w: path %q is a directory tracked in the parent; list files explicitly", errInvalidWriteSet, p)
		}
	}
	return nil
}

// resolveParent returns the lane's parent commit: the existing lane tip if
// refs/heads/loto/<ref> resolves (multi-wave), else base resolved to a SHA.
func (g gitRunner) resolveParent(ctx context.Context, ref, base string) (string, error) {
	laneRef := "refs/heads/loto/" + ref
	if out, err := g.run(ctx, gitCall{args: []string{gitRevParse, "--verify", "--quiet", laneRef + "^{commit}"}}); err == nil {
		if sha := strings.TrimSpace(out); sha != "" {
			return sha, nil
		}
	}
	out, err := g.run(ctx, gitCall{args: []string{gitRevParse, "--verify", "--quiet", base + "^{commit}"}})
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a commit: %w", errBadBase, base, err)
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", fmt.Errorf("%w: %q resolved empty", errBadBase, base)
	}
	return sha, nil
}

// laneIndexPath returns the per-lane index file path. It uses
// `git rev-parse --git-path` rather than a literal ".git/..." join because
// inside a linked worktree '.git' is a FILE, and loto may itself run in one.
func (g gitRunner) laneIndexPath(ctx context.Context, ref string) (string, error) {
	out, err := g.run(ctx, gitCall{args: []string{gitRevParse, "--git-path", "loto/idx/" + ref}})
	if err != nil {
		return "", fmt.Errorf("lane: resolve index path: %w", err)
	}
	p := strings.TrimSpace(out)
	if p == "" {
		return "", errIndexPath
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(g.repoTop, p)
	}
	return p, nil
}

func (o Opts) identityEnv() []string {
	return []string{
		"GIT_AUTHOR_NAME=" + o.Author.Name,
		"GIT_AUTHOR_EMAIL=" + o.Author.Email,
		"GIT_COMMITTER_NAME=" + o.Committer.Name,
		"GIT_COMMITTER_EMAIL=" + o.Committer.Email,
	}
}

func (o Opts) validate() error {
	switch {
	case o.RepoTop == "":
		return fmt.Errorf("%w: RepoTop", errMissingField)
	case o.Base == "":
		return fmt.Errorf("%w: Base", errMissingField)
	case o.Message == "":
		return fmt.Errorf("%w: Message", errMissingField)
	case o.Author.Name == "" || o.Author.Email == "":
		return fmt.Errorf("%w: Author name and email (commit-tree sets no identity)", errMissingField)
	case o.Committer.Name == "" || o.Committer.Email == "":
		return fmt.Errorf("%w: Committer name and email (commit-tree sets no identity)", errMissingField)
	}
	if err := validateRef(o.Ref); err != nil {
		return err
	}
	return validateWriteSet(o.RepoTop, o.WriteSet)
}

// validateRef rejects lane names that would break the ref path or smuggle a
// flag/pathspec into a git argument. Allowed: non-empty, ASCII letters, digits
// and -._/, with no leading '-', no '..', and no empty path segment.
func validateRef(ref string) error {
	switch {
	case ref == "":
		return fmt.Errorf("%w: empty", errInvalidRef)
	case strings.HasPrefix(ref, "-"):
		return fmt.Errorf("%w: %q starts with '-'", errInvalidRef, ref)
	case strings.Contains(ref, ".."):
		return fmt.Errorf("%w: %q contains '..'", errInvalidRef, ref)
	case strings.HasPrefix(ref, "/"), strings.HasSuffix(ref, "/"), strings.Contains(ref, "//"):
		return fmt.Errorf("%w: %q has an empty path segment", errInvalidRef, ref)
	}
	if i := strings.IndexFunc(ref, func(r rune) bool { return !isRefRune(r) }); i >= 0 {
		return fmt.Errorf("%w: %q has invalid character %q", errInvalidRef, ref, string(ref[i]))
	}
	return nil
}

func isRefRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return r == '-' || r == '_' || r == '.' || r == '/'
	}
}

// validateWriteSet requires at least one path and rejects any entry that is not
// a single literal repo-relative file. git reads the write-set as a NUL-delimited
// PATHSPEC list (--pathspec-from-file), so anything wider than an exact file
// re-opens the fs84 sweep this package exists to prevent. Rejected: empty,
// NUL-bearing, pathspec magic (leading ':'), a glob metacharacter (* ? [ ]),
// absolute, '..'-escaping, a trailing '/', or an entry that stats as a directory
// on disk.
//
// The directory checks are load-bearing, not belt-and-suspenders: buildLaneTree
// wraps each pathspec in :(literal), which neutralizes wildcards and magic but
// does NOT stop a directory pathspec from prefix-matching every file under it. A
// bare existing directory must be rejected here. A path absent from disk is
// allowed here — a removed FILE is a deletion the lane legitimately records under
// `git add -A`. A path tracked as a DIRECTORY in the parent is the same sweep
// vector yet invisible to this worktree-only check when removed (ENOENT) or
// shadowed by a regular file; buildLaneTree's rejectTrackedDirWriteSet catches it
// against the parent-seeded index, where os.Stat cannot.
func validateWriteSet(repoTop string, paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("%w: must list at least one path", errInvalidWriteSet)
	}
	for _, p := range paths {
		switch {
		case p == "":
			return fmt.Errorf("%w: empty path", errInvalidWriteSet)
		case strings.ContainsRune(p, '\x00'):
			return fmt.Errorf("%w: path %q contains NUL", errInvalidWriteSet, p)
		case strings.HasPrefix(p, ":"):
			return fmt.Errorf("%w: path %q uses pathspec magic (leading ':')", errInvalidWriteSet, p)
		case strings.ContainsAny(p, "*?[]"):
			return fmt.Errorf("%w: path %q contains a glob metacharacter", errInvalidWriteSet, p)
		case filepath.IsAbs(p):
			return fmt.Errorf("%w: path %q must be repo-relative, not absolute", errInvalidWriteSet, p)
		case p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/.."):
			return fmt.Errorf("%w: path %q escapes the repo", errInvalidWriteSet, p)
		case strings.HasSuffix(p, "/"):
			return fmt.Errorf("%w: path %q is a directory (trailing '/'); list files explicitly", errInvalidWriteSet, p)
		}
		if info, err := os.Stat(filepath.Join(repoTop, filepath.FromSlash(p))); err == nil && info.IsDir() {
			return fmt.Errorf("%w: path %q is a directory; list files explicitly", errInvalidWriteSet, p)
		}
	}
	return nil
}

type gitRunner struct{ repoTop string }

type gitCall struct {
	args  []string
	env   []string // extra entries appended to os.Environ()
	stdin string
}

// run executes one git command under gitTimeout, returning trimmed-nothing
// stdout. On a non-zero exit the error carries the command and its stderr.
func (g gitRunner) run(ctx context.Context, c gitCall) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	//nolint:gosec // G204: the command is the literal "git"; args are internal plumbing tokens (validated ref names and write-set pathspecs), never shell-interpreted.
	cmd := exec.CommandContext(ctx, "git", c.args...)
	cmd.Dir = g.repoTop
	if len(c.env) > 0 {
		cmd.Env = append(os.Environ(), c.env...)
	}
	if c.stdin != "" {
		cmd.Stdin = strings.NewReader(c.stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(c.args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
