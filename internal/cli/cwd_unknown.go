package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
)

// refuseUnresolvableRelative implements --cwd-unknown (loto-tzmv.10): when the
// caller's working directory is not knowable here, a relative token has no
// single meaning and MUST NOT be resolved.
//
// The concrete failure it closes: mcp__trixi__agent_shell keeps its own
// persistent cwd, set by a `cd` that may have happened three tool calls ago,
// and that cwd is absent from the PreToolUse payload. A bare relative token
// therefore has three candidate bases — the payload's cwd, the shell's actual
// cwd, and the repo root — and resolveCLITarget silently picks the third
// (normalizeRepoPath returns a relative path unchanged, so Canonicalize treats
// it as repo-relative). Reproduced 2026-08-14: a peer holds
// internal/store/locks.go, `loto check locks.go` from the repo root prints
// "✓ no conflicts", exit 0, and an agent_shell sitting in internal/store is
// waved through a `mv locks.go locks_old.go`.
//
// ‡ This is NOT fail-open. Fail-open declines to answer; the old behavior
// returned a POSITIVE clean verdict that was false, which is strictly worse
// than no check at all because it reads as protection. Refusing is the whole
// point — the alternatives (ask the trixi daemon for the shell's cwd; mandate
// absolute paths by convention) were rejected as a hot-path dependency and a
// rule agents violate silently.
//
// ‡ The resolution rule DIFFERS per tool and the difference is load-bearing.
// For Bash, the payload cwd plus any leading `cd` in the same command is sound
// because the process is fresh per call. For agent_shell it is not. One
// implementation must not drift across both call sites, which is why the
// caller declares "cwd unknown" rather than this code sniffing a tool name.
//
// Absolute paths are unaffected: they carry their own base. Returns
// refused=true (and the exit code to use) only when at least one relative
// token was seen; otherwise the caller proceeds normally.
func refuseUnresolvableRelative(stdout io.Writer, paths []string) (rc int, refused bool) {
	var rel []string
	for _, p := range paths {
		if p != "" && !filepath.IsAbs(p) {
			rel = append(rel, p)
		}
	}
	if len(rel) == 0 {
		return 0, false
	}
	sort.Strings(rel)

	fmt.Fprintf(stdout, "✗ unresolvable count=%d\n", len(rel))
	for _, p := range rel {
		fmt.Fprintf(stdout, "✗ path=%s reason=relative-path-caller-cwd-unknown\n", p)
	}
	fmt.Fprintln(stdout, "ℹ the caller's working directory is not in the hook payload, so a relative path has no single meaning here")
	fmt.Fprintln(stdout, "```bash")
	fmt.Fprintln(stdout, "# re-run the command with absolute paths, or cd and use paths from the repo root")
	// Quoted: a checkout path containing a space or a glob character would
	// otherwise word-split or expand after the reader substitutes <path>, so
	// the copy-paste fix would check different arguments than it names
	// (Codex #247).
	fmt.Fprintf(stdout, "loto check %q\n", filepath.Join("$(git rev-parse --show-toplevel)", "<path>"))
	fmt.Fprintln(stdout, "```")
	return 1, true
}
