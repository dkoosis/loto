package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"loto/internal/store"
)

func init() { register("tag", cmdTag) } //nolint:gochecknoinits // command registry pattern

func cmdTag(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tag", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(stderr, `usage: loto tag <file> <text...>`)
		return 2
	}
	raw := fs.Arg(0)
	text := strings.TrimSpace(strings.Join(fs.Args()[1:], " "))
	if text == "" {
		fmt.Fprintln(stderr, "✗ tag text required")
		return 2
	}

	repoTop, _ := repoTopForCwd(ctx)
	target, err := resolveCLITarget(repoTop, raw)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 2
	}

	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	host, err := rt.Store.LockAt(rt.Ctx, target)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if host == nil {
		fmt.Fprintf(stderr, "✗ %s not locked — acquire it yourself\n", relPath(target.Canonical))
		return 3
	}

	id, err := rt.Store.InsertTag(rt.Ctx, store.NewTag{
		TargetCanonical: target.Canonical,
		LockOwnerUUID:   host.OwnerUUID,
		LockCreatedAt:   host.CreatedAt.UnixNano(),
		TaggerUUID:      rt.Agent.UUID,
		Text:            text,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrTagCapReached):
			fmt.Fprintf(stderr, "✗ tag cap reached on %s (5) — escalate channel\n", relPath(target.Canonical))
			return 3
		case errors.Is(err, store.ErrNoHostLock):
			// Race: lock dropped between LockAt and InsertTag.
			fmt.Fprintf(stderr, "✗ %s not locked — acquire it yourself\n", relPath(target.Canonical))
			return 3
		default:
			fmt.Fprintf(stderr, "✗ %v\n", err)
			return 3
		}
	}
	fmt.Fprintf(stdout, "✓ tag id=%s target=%s\n", id, relPath(target.Canonical))
	return 0
}
