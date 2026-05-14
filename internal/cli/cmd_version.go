package cli

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
)

func init() { register("version", cmdVersion) } //nolint:gochecknoinits // command registry pattern

func cmdVersion(_ context.Context, _ []string, stdout, _ io.Writer) int {
	rev, when := "unknown", "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				when = s.Value
			}
		}
	}
	fmt.Fprintf(stdout, "loto rev=%s built=%s\n", rev, when)
	return 0
}
