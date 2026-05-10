package domain

import "time"

// PidLiveProbe returns true if (host,pid) is currently running.
type PidLiveProbe func(host string, pid int) bool

// IsStale returns true if the lock is past its TTL OR the holder pid is provably
// dead on this host. Cross-host pid checks are out of scope.
func IsStale(l LockRecord, now time.Time, thisHost string, live PidLiveProbe) bool {
	if !now.Before(l.ExpiresAt) {
		return true
	}
	if l.Host == thisHost && !live(l.Host, l.PID) {
		return true
	}
	return false
}
