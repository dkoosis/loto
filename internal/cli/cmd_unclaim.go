package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"loto/internal/domain"
	"loto/internal/render"
	"loto/internal/store"
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
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: loto unclaim <path-prefix>")
		return 2
	}
	repoTop, _ := repoTopForCwd(ctx)
	prefix, err := resolveCLIPrefix(repoTop, flags.Arg(0))
	if err != nil {
		render.EmitInvalid(stderr, []render.InvalidTarget{{Path: flags.Arg(0), Reason: classifyCanonicalizeErr(err)}})
		return 2
	}

	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	defer rt.DeferredTagFooter(stdout)

	res, err := rt.Store.ReleaseClaim(rt.Ctx, prefix.Canonical, domain.AgentUUID(rt.Agent.UUID))
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	switch res.State {
	case store.ClaimStateReleased:
		fmt.Fprintf(stdout, "✓ unclaimed count=1\n✓ prefix=%s\n", res.PathPrefix)
		return 0
	case store.ClaimStateNoClaim:
		fmt.Fprintf(stdout, "✓ unclaimed count=0\nℹ prefix=%s state=no-claim\n", res.PathPrefix)
		return 0
	case store.ClaimStateNotOwner:
		fmt.Fprintf(stdout, "✓ unclaimed count=0\n✗ prefix=%s state=not-owner owner=%s\n", res.PathPrefix, res.Owner)
		return 1
	default:
		fmt.Fprintf(stderr, "✗ unexpected release state %d\n", res.State)
		return 3
	}
}
