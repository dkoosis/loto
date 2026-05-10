//go:build !unix

package cli

// pidLive: non-Unix fallback. Always reports alive — TTL remains authoritative.
func pidLive(pid int) bool { return true }
