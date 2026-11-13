//go:build !darwin

package cli

import (
	"fmt"
	"os"
)

// syncFull forces f to stable storage; plain fsync is the correct barrier on
// non-darwin platforms. φ durable_sync_darwin.go.
func syncFull(f *os.File) error {
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync file data: %w", err)
	}
	return nil
}
