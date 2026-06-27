package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"loto/internal/domain"
	"loto/internal/store"
)

func init() { register("ack", cmdAck) } //nolint:gochecknoinits // command registry pattern

func cmdAck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: loto ack <tag-id>")
		return 2
	}
	tagID := fs.Arg(0)

	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	if err := rt.Store.Ack(rt.Ctx, tagID, domain.AgentUUID(rt.Agent.UUID)); err != nil {
		if errors.Is(err, store.ErrTagNotMine) {
			fmt.Fprintln(stderr, "✗ not addressed to you")
			return 3
		}
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	fmt.Fprintf(stdout, "✓ ack id=%s\n", tagID)
	return 0
}
