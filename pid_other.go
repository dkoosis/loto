//go:build !unix

package loto

// pidAlive is a best-effort stub on non-unix platforms.
// Returns true (assume alive) to avoid false positives.
func pidAlive(pid int) bool {
	return pid > 0
}
