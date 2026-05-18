package cli

import (
	"context"
	"io"
)

// Run is a test-only shim around RunContext with a background context.
// Production code calls RunContext directly (see cmd/loto/main.go).
func Run(argv []string, stdout, stderr io.Writer) int {
	return RunContext(context.Background(), argv, stdout, stderr)
}
