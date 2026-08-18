package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"loto/internal/domain"
	"loto/internal/store"
)

func init() { register("ack", cmdAck) } //nolint:gochecknoinits // command registry pattern

// territoryTagIDPrefix is what makes `loto ack <id>` route without a flag or a
// DB round-trip. Held-tag ids are `t-`+hex; territory-tag ids are `tt-`+hex.
const territoryTagIDPrefix = "tt-"

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

	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	// Route by id prefix (loto-z3y1 D5): one verb, one vocabulary, no new flag,
	// and the decision costs no lookup. `tt-` ids are minted only by
	// InsertTerritoryTag, so the branch cannot be ambiguous.
	if strings.HasPrefix(tagID, territoryTagIDPrefix) {
		if err := rt.Store.AckTerritoryTag(rt.Ctx, tagID, rt.Agent.UUID); err != nil {
			fmt.Fprintf(stderr, "✗ %v\n", err)
			return 3
		}
		fmt.Fprintf(stdout, "✓ ack id=%s\n", tagID)
		return 0
	}

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
