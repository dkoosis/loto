package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"loto/internal/render"
	"loto/internal/store"
)

func init() { register("downgrade", cmdDowngrade) } //nolint:gochecknoinits // command registry pattern

const downgradeUsageHead = `usage: loto downgrade <target> [<target>...]

Downgrade your exclusive lock(s) to shared, in place — peers may then take
shared locks on the same target without you releasing first. Restores the
file's write bit. A lock that is already shared is a no-op.
`

func cmdDowngrade(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("downgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, downgradeUsageHead) }
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprint(stderr, downgradeUsageHead)
		return 2
	}
	repoTop, _ := repoTopForCwd(ctx)
	targets, invalid := validateLockTargets(fs.Args(), repoTop)
	if len(invalid) > 0 {
		render.EmitInvalid(stderr, invalid)
		return 2
	}
	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	exit := 0
	for _, t := range targets {
		switch err := rt.Store.DowngradeLock(rt.Ctx, t, rt.Agent.UUID); {
		case err == nil:
			fmt.Fprintf(stdout, "✓ downgraded target=%s mode=shared\n", relPath(t.Canonical))
		case errors.Is(err, store.ErrNoLockAtTarget):
			fmt.Fprintf(stdout, "✗ target=%s reason=no-lock-held\n", relPath(t.Canonical))
			exit = 1
		default:
			var cfe *store.ChmodFailureError
			if errors.As(err, &cfe) {
				// Mode flipped to shared in the DB, but the write-bit restore
				// failed — surface as ✗ (closed glyph vocabulary, design.md).
				fmt.Fprintf(stdout, "✗ target=%s mode=shared reason=write-bit-restore-failed\n", relPath(t.Canonical))
				exit = 3
				continue
			}
			fmt.Fprintf(stderr, "✗ %v\n", err)
			exit = 3
		}
	}
	return exit
}
