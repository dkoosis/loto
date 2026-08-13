package identity

import (
	"os"
	"strings"
)

// osHostname is the test seam for the OS lookup, matching the procArgv/killFn
// pattern used elsewhere in this package.
var osHostname = os.Hostname

// HostID resolves the name that scopes a lock record to one machine. ok=false
// means this process has no verifiable host identity.
//
// LOTO_HOST is the operator escape hatch and wins outright: setting it restores
// full pid-based stale-lock reclaim on a box whose os.Hostname() is broken.
// Setting the SAME value on two machines that share a repo db makes each treat
// the other's locks as local and pid-probe pid numbers belonging to a different
// kernel — so the value must be per-machine stable and per-machine unique.
//
// An empty hostname is reported as unknown rather than passed through as "".
// Callers compare a lock's recorded host against this one; "" would compare
// equal to another host-less machine's records and unequal to every real one,
// which is worse than admitting the answer is unavailable.
func HostID() (string, bool) {
	if v := strings.TrimSpace(os.Getenv("LOTO_HOST")); v != "" {
		return v, true
	}
	h, err := osHostname()
	if err != nil || h == "" {
		return "", false
	}
	return h, true
}
