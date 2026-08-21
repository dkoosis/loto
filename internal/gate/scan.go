package gate

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // matches git's own blob-hash algorithm, not used for security
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ErrUnhashablePath reports a repo path the fingerprinter cannot feed through
// `git hash-object --stdin-paths`, which reads newline-terminated paths.
var ErrUnhashablePath = errors.New("gate: scan: path contains a newline, cannot fingerprint")

// ErrHashCountMismatch reports hash-object returning a different number of
// hashes than paths — the pairing is positional, so a mismatch would silently
// attach the wrong fingerprint to every path after it.
var ErrHashCountMismatch = errors.New("gate: scan: hash-object returned the wrong number of hashes")

// ErrNoBaseline reports that refs/loto/integration does not currently
// resolve. This is NOT evidence the tree agrees with anything — a baseline
// that never existed and a baseline that existed, had violations recorded
// against it, and then vanished (a deleted ref, a corrupt object store) are
// indistinguishable at this call, and neither licenses treating every open
// violation as reverted. The caller owns that distinction (Codex #276 P1).
var ErrNoBaseline = errors.New("gate: scan: refs/loto/integration does not resolve")

// Observation is one worktree path whose content no longer matches the
// integration ref. It is a raw sensor reading, NOT a violation: the sensor
// cannot see lease state, and a leaseholder editing its own leased file is
// exactly this shape. Deciding which observations are violations belongs to
// the caller, which holds the store (git-gate.md Phase 5: "the watcher
// optimizes feedback; the gate provides correctness").
//
// ‡ Keeping the verdict out of this package preserves gate's store-freedom,
// the same invariant admission.go states for AdmitParams.CurrentEpoch: this
// package reads git and nothing else.
type Observation struct {
	Path string
	// Fingerprint is the blob SHA of the worktree content as it stands now —
	// empty when Deleted. Recorded so a violation row carries WHAT was seen,
	// not merely that something was, and so a later scan can tell a second
	// rogue write from the first one it already recorded.
	Fingerprint string
	// Deleted: the path exists in integration and is gone from the worktree.
	// A deletion is as much an unauthorized mutation as an edit, and the
	// admission check treats it identically — but it has no content to hash.
	Deleted bool
}

// ScanWorktree returns every tracked path whose worktree content differs from
// refs/loto/integration, sorted by path.
//
// Read-only and best-effort by construction:
//
//   - No integration ref yet → (nil, ErrNoBaseline). The sensor never
//     bootstraps the ref; ResolveIntegrationRef does that, and it is a
//     WRITE, which a scan fired from a PreToolUse hook has no business
//     performing. ErrNoBaseline is distinct from a clean empty reading
//     precisely so a caller cannot mistake "nothing to compare against" for
//     "compared, found nothing" — only the latter may auto-resolve.
//   - Untracked files are deliberately out of scope. They are absent from
//     integration, so "differs" is not a meaningful reading of them —
//     untracked residue is its own problem (loto-ovno.13).
//   - Renames are disabled (--no-renames) so every changed path reports as a
//     plain add/modify/delete. A rename reported as one R record would hide
//     the destination path from the intersect check, which is precisely the
//     path a candidate would go on to declare.
func ScanWorktree(ctx context.Context, repoTop string) ([]Observation, error) {
	g := gitRunner{repoTop: repoTop}
	if _, err := g.run(ctx, "rev-parse", "--verify", "--quiet", IntegrationRef); err != nil {
		return nil, ErrNoBaseline
	}
	out, err := g.run(ctx, "diff", "--name-status", "-z", "--no-renames", IntegrationRef, "--")
	if err != nil {
		return nil, fmt.Errorf("gate: scan diff against %s: %w", IntegrationRef, err)
	}
	changed, deleted := parseNameStatusZ(out)
	fingerprints, err := hashWorktreePaths(ctx, repoTop, changed)
	if err != nil {
		return nil, err
	}

	obs := make([]Observation, 0, len(changed)+len(deleted))
	for _, p := range changed {
		obs = append(obs, Observation{Path: p, Fingerprint: fingerprints[p]})
	}
	for _, p := range deleted {
		obs = append(obs, Observation{Path: p, Deleted: true})
	}
	sort.Slice(obs, func(i, j int) bool { return obs[i].Path < obs[j].Path })
	return obs, nil
}

// parseNameStatusZ splits `git diff --name-status -z` output — NUL-separated
// records alternating status letter and path — into present-in-worktree paths
// and deleted ones. -z rather than the line format because a repo path may
// contain anything but NUL, and the line format would quote-escape it into
// something this parser would have to un-escape correctly to stay honest.
func parseNameStatusZ(out string) (changed, deleted []string) {
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		status, path := fields[i], fields[i+1]
		if status == "" || path == "" {
			continue
		}
		if status[0] == 'D' {
			deleted = append(deleted, path)
			continue
		}
		changed = append(changed, path)
	}
	return changed, deleted
}

// hashWorktreePaths returns path -> worktree blob SHA for paths, in ONE
// `git hash-object --stdin-paths` call rather than one process per path: a
// dirty tree mid-wave routinely carries dozens of changed files and this runs
// on the PreToolUse path, where per-call cost is paid on every tool use.
//
// --stdin-paths reads newline-terminated paths, so a path containing a
// newline cannot be fed through it. Rather than silently mis-hash the
// following path, such a path is refused by name — a repo that has one gets
// an error it can act on, not a wrong fingerprint it cannot see.
//
// Symlinks are hashed separately, in Go, from their link payload rather than
// through hash-object --stdin-paths (Codex #276 P2): that command opens the
// path, which for a symlink means following it, and a symlink retargeted to
// a dangling destination makes the OPEN fail with exit 128 — for the WHOLE
// batch, not just that one path, so one stale symlink would silently drop
// every other violation the same scan should have recorded. Git itself never
// dereferences a symlink to hash it — the blob IS the link target string —
// so gitBlobSHA reproduces that directly from os.Readlink, no subprocess,
// nothing to open.
func hashWorktreePaths(ctx context.Context, repoTop string, paths []string) (map[string]string, error) {
	if len(paths) == 0 {
		return map[string]string{}, nil
	}
	for _, p := range paths {
		if strings.ContainsAny(p, "\n\r") {
			return nil, fmt.Errorf("%w: %q", ErrUnhashablePath, p)
		}
	}
	m := make(map[string]string, len(paths))
	regular := make([]string, 0, len(paths))
	for _, p := range paths {
		fi, err := os.Lstat(filepath.Join(repoTop, p))
		if err != nil {
			// Vanished between the diff and here (or otherwise unreadable):
			// not this function's failure mode to invent a fingerprint for.
			return nil, fmt.Errorf("gate: scan: lstat %q: %w", p, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			regular = append(regular, p)
			continue
		}
		target, err := os.Readlink(filepath.Join(repoTop, p))
		if err != nil {
			return nil, fmt.Errorf("gate: scan: readlink %q: %w", p, err)
		}
		m[p] = gitBlobSHA(target)
	}
	if len(regular) == 0 {
		return m, nil
	}

	cmd := exec.CommandContext(ctx, "git", "hash-object", "--stdin-paths")
	cmd.Dir = repoTop
	cmd.Stdin = strings.NewReader(strings.Join(regular, "\n") + "\n")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gate: scan hash-object: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	shas := strings.Fields(out.String())
	if len(shas) != len(regular) {
		return nil, fmt.Errorf("%w: %d hashes for %d paths", ErrHashCountMismatch, len(shas), len(regular))
	}
	for i, p := range regular {
		m[p] = shas[i]
	}
	return m, nil
}

// gitBlobSHA computes the same SHA-1 `git hash-object` would for a blob
// holding content — "blob <len>\x00<content>" — without shelling out. Used
// only for symlink targets, where content is the link payload string.
func gitBlobSHA(content string) string {
	h := sha1.New() //nolint:gosec // git's blob object id, not a security use
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}
