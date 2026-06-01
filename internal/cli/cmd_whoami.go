package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"loto/internal/identity"
)

func init() { register("whoami", cmdWhoami) } //nolint:gochecknoinits // command registry pattern

func cmdWhoami(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit identity as a single JSON object (uuid/handle/host)")
	// --ensure is the historical hook flag; identity.Ensure always runs, so it
	// is accepted as a no-op for back-compat with the SessionStart hook.
	_ = fs.Bool("ensure", false, "ensure an identity exists (no-op: always ensured)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}

	a, err := identity.Ensure(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ identity: %v\n", err)
		return 3
	}

	if *asJSON {
		// Emit only the identity fields the SessionStart hook consumes. The key
		// for the agent id is "uuid" (matches identity.Agent json tags), so the
		// hook must read d["uuid"], not d["id"] (loto-u7b7).
		enc := json.NewEncoder(stdout)
		if encErr := enc.Encode(struct {
			UUID   string `json:"uuid"`
			Handle string `json:"handle"`
			Host   string `json:"host"`
		}{a.UUID, a.Handle, a.Host}); encErr != nil {
			fmt.Fprintf(stderr, "✗ encode: %v\n", encErr)
			return 3
		}
		return 0
	}

	fmt.Fprintf(stdout, "handle: %s\nuuid:   %s\nhost:   %s\n", a.Handle, a.UUID, a.Host)
	return 0
}
