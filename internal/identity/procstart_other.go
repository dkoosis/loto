//go:build !linux && !darwin

package identity

// ProcStart: fallback for OSes without a start-time reader. Always UNKNOWN —
// the caller's liveness check degrades to a plain pid-alive check (no
// PID-reuse protection), with TTL remaining authoritative. Mirrors
// pid_other.go's degrade-gracefully stance.
func ProcStart(pid int) (int64, bool) { return 0, false }
