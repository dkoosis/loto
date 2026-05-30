package domain

import "time"

// PidLiveProbe returns true if (host,pid) is currently running. storedStart is
// the lock's persisted holder start-time (0 = unknown): when nonzero, the real
// probe reads the current occupant's start-time and reports the pid dead on a
// mismatch, defeating PID reuse (loto-kwlp). Unknown (0) degrades to a plain
// pid-alive check.
type PidLiveProbe func(host string, pid int, storedStart int64) bool

// IsStale returns true if the lock is past its TTL OR the holder pid is provably
// dead on this host. Cross-host pid checks are out of scope.
func IsStale(l LockRecord, now time.Time, thisHost string, live PidLiveProbe) bool {
	if !now.Before(l.ExpiresAt) {
		return true
	}
	if l.Host == thisHost && !live(l.Host, l.PID, l.ProcStart) {
		return true
	}
	return false
}
