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

Run a broad-repo command against <commit-ish> in a throwaway detached worktree
cut off the lane ref, then remove that worktree. A non-zero command exit is a
test failure (exit 1); a setup/teardown/ctx failure is infra (exit 3).

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
	if res.Passed {
		fmt.Fprintf(w, "✓ verify passed commit=%s\n", commit)
		if body != "" {
			fmt.Fprintln(w, body)
		}
		return 0
	}
	fmt.Fprintf(w, "✗ verify failed commit=%s\n", commit)
	if body != "" {
		fmt.Fprintln(w, body)
	}
	return 1
}
