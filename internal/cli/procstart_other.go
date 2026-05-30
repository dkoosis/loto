//go:build !linux && !darwin

package cli

// procStart: fallback for OSes without a start-time reader. Always UNKNOWN —
// IsStale degrades to a plain pid-alive check (no PID-reuse protection), with
// TTL remaining authoritative. Mirrors pid_other.go's degrade-gracefully stance.
func procStart(pid int) (int64, bool) { return 0, false }
