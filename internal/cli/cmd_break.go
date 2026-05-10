package cli

import (
	"flag"
	"fmt"
	"io"

	"loto/internal/domain"
)

func init() { register("break", cmdBreak) } //nolint:gochecknoinits // command registry pattern

func cmdBreak(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("break", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "break a live lock")
	reason := fs.String("reason", "", "required: why this break")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *reason == "" {
		fmt.Fprintln(stderr, `usage: loto break <target> [--force] --reason "..."`)
		return 2
	}
	target, err := domain.Canonicalize(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 2
	}
	rt, err := openRuntime()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	live := func(host string, pid int) bool {
		if host != rt.Host {
			return true
		}
		return pidLive(pid)
	}
	if err := rt.Store.BreakLock(rt.Ctx, target, rt.Agent.UUID, *force, *reason, live); err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "✓ broken target=%s\n", target.Canonical)
	return 0
}
