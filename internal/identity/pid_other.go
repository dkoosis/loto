//go:build !unix

package identity

// PIDAlive: non-Unix fallback. Always reports alive — TTL remains authoritative.
func PIDAlive(pid int) bool { return true }
