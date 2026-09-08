package cli

import (
	"context"
	"fmt"
	"io"
)

func init() { register("version", cmdVersion) } //nolint:gochecknoinits // command registry pattern

func cmdVersion(_ context.Context, _ []string, stdout, _ io.Writer) int {
	rev, when := readBuildIdentity().display()
	fmt.Fprintf(stdout, "loto rev=%s built=%s\n", rev, when)
	return 0
}
