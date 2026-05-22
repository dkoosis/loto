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
	text := strings.TrimSpace(strings.Join(fs.Args()[1:], " "))
	if text == "" {
		fmt.Fprintln(stderr, "✗ tag text required")
		return 2
	}
	repoTop, _ := repoTopForCwd(ctx)
	target, err := resolveCLITarget(repoTop, fs.Arg(0))
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
	return runTag(rt, target.Canonical, text, stdout, stderr)
}

func runTag(rt *runtime, canonical, text string, stdout, stderr io.Writer) int {
	host, err := rt.Store.LockAt(rt.Ctx, domain.Target{Canonical: canonical})
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if host == nil {
		fmt.Fprintf(stderr, "✗ %s not locked — acquire it yourself\n", relPath(canonical))
		return 3
	}
	id, err := rt.Store.InsertTag(rt.Ctx, store.NewTag{
		TargetCanonical: canonical,
		LockOwnerUUID:   host.OwnerUUID,
		LockCreatedAt:   host.CreatedAt.UnixNano(),
		TaggerUUID:      rt.Agent.UUID,
		Text:            text,
	})
	if err != nil {
		return tagInsertErr(err, canonical, stderr)
	}
	fmt.Fprintf(stdout, "✓ tag id=%s target=%s\n", id, relPath(canonical))
	return 0
}

func tagInsertErr(err error, canonical string, stderr io.Writer) int {
	switch {
	case errors.Is(err, store.ErrTagCapReached):
		fmt.Fprintf(stderr, "✗ tag cap reached on %s (5) — escalate channel\n", relPath(canonical))
	case errors.Is(err, store.ErrNoHostLock):
		// Race: lock dropped between LockAt and InsertTag.
		fmt.Fprintf(stderr, "✗ %s not locked — acquire it yourself\n", relPath(canonical))
	default:
		fmt.Fprintf(stderr, "✗ %v\n", err)
	}
	return 3
}
