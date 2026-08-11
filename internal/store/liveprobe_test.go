package store

import "loto/internal/domain"

// tcHost is the host every seeded test LockRecord carries (mkFileLock et al) —
// the "local machine" these tests evaluate liveness from.
const tcHost = "h"

// hostPidProbe adapts an old-style pid predicate to the HolderLiveProbe
// trichotomy, preserving the IsStale contract these tests were written against
// (pre-loto-ygty, when the store took a thisHost string and the probe took
// (host, pid, storedStart)): a holder on another host, or one carrying the
// PID-0 sentinel, has no durable liveness handle → UNKNOWN, so TTL is the sole
// authority. Only a local durable pid consults pidAlive — true → ALIVE,
// false → DEAD.
func hostPidProbe(thisHost string, pidAlive func(pid int, storedStart int64) bool) domain.HolderLiveProbe {
	return func(l domain.LockRecord) domain.Liveness {
		if l.Host != thisHost || l.PID <= 0 {
			return domain.LivenessUnknown
		}
		if pidAlive(l.PID, l.ProcStart) {
			return domain.LivenessAlive
		}
		return domain.LivenessDead
	}
}

// aliveOn/deadOn are the two constant probes, evaluated from thisHost's
// perspective — the scenario's "which machine am I asking from".
func aliveOn(thisHost string) domain.HolderLiveProbe {
	return hostPidProbe(thisHost, func(int, int64) bool { return true })
}

func deadOn(thisHost string) domain.HolderLiveProbe {
	return hostPidProbe(thisHost, func(int, int64) bool { return false })
}

// liveProbe reports every local pid alive — keeps seeded holders non-stale so
// the mode predicate (not reclaim) governs coexistence. deadProbe is its
// mirror: every local pid gone, so only the PID-0/cross-host escapes survive.
var (
	liveProbe = aliveOn(tcHost)
	deadProbe = deadOn(tcHost)
)
