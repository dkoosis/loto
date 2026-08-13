package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"loto/internal/domain"
	"loto/internal/render"
)

func init() { register("unclaim", cmdUnclaim) } //nolint:gochecknoinits // command registry pattern

// unclaimUsageHead mirrors claimUsageHead for the symmetric verb. No -t:
// releasing your own reservation needs no justification (cf. plain unlock,
// loto-e0mz).
const unclaimUsageHead = `usage: loto unclaim <path-prefix>

Release your claim at exactly this prefix. Only the claim's owner can release
it; another agent's claim reports ✗ not-owner and stays in place.

examples:
  loto unclaim internal/store
`

func cmdUnclaim(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("unclaim", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprint(stderr, unclaimUsageHead)
		flags.PrintDefaults()
	}
	if err := flags.Parse(permuteWith(flags, args)); err != nil {
		return 2
	}
	prefix, _, code := claimVerbPrefix(ctx, flags, "usage: loto unclaim <path-prefix>", stderr)
	if code != 0 {
		return code
	}
	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	defer rt.DeferredTagFooter(stdout)
	defer rt.DeferredMailFooter(stdout)

	res, err := rt.Store.ReleaseClaim(rt.Ctx, prefix.Canonical, domain.AgentUUID(rt.Agent.UUID))
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	return render.EmitClaimRelease(stdout, res)
}
