package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"loto/internal/lane"
)

func init() { register("verify", cmdVerify) } //nolint:gochecknoinits // command registry pattern

const verifyUsageHead = `usage: loto verify <commit-ish> -- <cmd> [args...]

Run a broad-repo command against <commit-ish> in a detached worktree holding
exactly that commit, never the shared checkout. The worktree is reused across
runs (one per repo, under .git/loto-verify) and reset to <commit-ish> first, so
what the command sees matches a fresh cut. A non-zero command exit is a test
failure (exit 1); a setup/teardown/ctx failure is infra (exit 3).

examples:
  loto verify loto/impl-1 -- go test -race ./...
  loto verify HEAD -- go vet ./...
`

func cmdVerify(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, verifyUsageHead)
		fs.PrintDefaults()
	}
	// No permuteWith: everything after the commit is the verify command, which
	// carries its own flags (e.g. -race) that must not be parsed as loto flags.
	// flag.Parse stops at the first non-flag token (the commit), leaving the rest
	// in fs.Args().
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(stderr, verifyUsageHead)
		return 2
	}
	commit := rest[0]
	cmd := rest[1:]
	// Drop an optional "--" separator between the commit and the command.
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		fmt.Fprintln(stderr, "✗ verify command required: loto verify <commit-ish> -- <cmd> [args...]")
		return 2
	}

	repoTop, err := repoTopForCwd(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	res, err := lane.Verify(ctx, repoTop, commit, cmd)
	if err != nil {
		// Could not RUN to a verdict (worktree setup/teardown, command start, or
		// ctx expiry) — infra, distinct from a failing test.
		fmt.Fprintf(stderr, "✗ verify aborted commit=%s: %v\n", commit, err)
		return 3
	}
	return emitVerifyResult(stdout, commit, res)
}

// emitVerifyResult renders the verify verdict Claude-optimized: a glyph triage
// line first, then the command's path-scrubbed output. Returns the exit code:
// 0 passed, 1 failed.
func emitVerifyResult(w io.Writer, commit string, res lane.VerifyResult) int {
	body := strings.TrimRight(res.Output, "\n")
	code := 1
	glyph, verdict := "✗", "failed"
	if res.Passed {
		code, glyph, verdict = 0, "✓", "passed"
	}
	fmt.Fprintf(w, "%s verify %s commit=%s\n", glyph, verdict, commit)
	if row := verifyTreeRow(res.Tree, res.TreeReason); row != "" {
		fmt.Fprintln(w, row)
	}
	if body != "" {
		fmt.Fprintln(w, body)
	}
	return code
}

// verifyTreeRow reports which checkout ran the verify. Always emitted, because
// the failure it exists to catch is a SILENT one: a repo that has quietly
// fallen back to cutting a worktree per verify pays seconds per promotion and
// looks, in every other output, exactly like a repo on the fast path.
// ℹ for reuse (neutral data), ⚠ for anything slower, which is advisory and
// never fatal. Empty mode means a hand-built VerifyResult, not a fault, so it
// stays silent.
func verifyTreeRow(mode lane.VerifyTreeMode, reason string) string {
	if mode == "" {
		return ""
	}
	glyph := "⚠"
	if mode == lane.TreeReuse {
		glyph = "ℹ"
	}
	row := fmt.Sprintf("%s verify tree=%s", glyph, mode)
	if reason != "" {
		row += " reason=" + reason
	}
	return row
}
