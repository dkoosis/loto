package cli

import (
	"context"
	"fmt"
	"io"

	"loto/internal/identity"
)

func init() { register("whoami", cmdWhoami) } //nolint:gochecknoinits // command registry pattern

func cmdWhoami(ctx context.Context, _ []string, stdout, stderr io.Writer) int {
	a, err := identity.Ensure(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ identity: %v\n", err)
		return 3
	}
	fmt.Fprintf(stdout, "handle: %s\nuuid:   %s\nhost:   %s\n", a.Handle, a.UUID, a.Host)
	return 0
}
