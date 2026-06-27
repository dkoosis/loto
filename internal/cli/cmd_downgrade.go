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

	// Batch all targets through one op-flock + one write tx (loto-r2wc): a
	// multi-target downgrade no longer pays N op-flock acquire/release cycles
	// and N fsyncs serialized against every live peer process.
	results, err := rt.Store.DowngradeLocks(rt.Ctx, targets, rt.Agent.UUID)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	exit := 0
	for i := range results {
		r := &results[i]
		switch {
		case r.Err == nil && r.RestoreErr == nil:
			fmt.Fprintf(stdout, "✓ downgraded target=%s mode=shared\n", relPath(r.Target.Canonical))
		case errors.Is(r.Err, store.ErrNoLockAtTarget):
			fmt.Fprintf(stdout, "✗ target=%s reason=no-lock-held\n", relPath(r.Target.Canonical))
			exit = 1
		case r.RestoreErr != nil:
			// Mode flipped to shared in the DB, but the write-bit restore
			// failed — surface as ✗ (closed glyph vocabulary, design.md).
			fmt.Fprintf(stdout, "✗ target=%s mode=shared reason=write-bit-restore-failed\n", relPath(r.Target.Canonical))
			exit = 3
		default:
			fmt.Fprintf(stderr, "✗ %v\n", r.Err)
			exit = 3
		}
	}
	return exit
}
