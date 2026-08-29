//go:build !darwin

package cli

import "os"

// syncFull forces f to stable storage; plain fsync is the correct barrier on
// non-darwin platforms. φ durable_sync_darwin.go.
func syncFull(f *os.File) error {
	return f.Sync()
}
