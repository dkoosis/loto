package domain

// hostPidProbe adapts an old-style pid predicate to the HolderLiveProbe
// trichotomy, preserving the IsStale contract these tests were written against
// (pre-loto-ygty, when EvalContext carried ThisHost and the probe took
// (host, pid, storedStart)): a holder on another host, or one carrying the
// PID-0 sentinel, has no durable liveness handle → UNKNOWN, so TTL is the sole
// authority. Only a local durable pid consults pidAlive — true → ALIVE,
// false → DEAD.
func hostPidProbe(thisHost string, pidAlive func(pid int, storedStart int64) bool) HolderLiveProbe {
	return func(l LockRecord) Liveness {
		if l.Host != thisHost || l.PID <= 0 {
			return LivenessUnknown
		}
		if pidAlive(l.PID, l.ProcStart) {
			return LivenessAlive
		}
		return LivenessDead
	}
}

// aliveOn/deadOn are the two constant probes, evaluated from thisHost's
// perspective — the scenario's "which machine am I asking from".
func aliveOn(thisHost string) HolderLiveProbe {
	return hostPidProbe(thisHost, func(int, int64) bool { return true })
}

func deadOn(thisHost string) HolderLiveProbe {
	return hostPidProbe(thisHost, func(int, int64) bool { return false })
}
