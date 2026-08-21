package domain

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrMalformedHoldRef is returned by ParseHoldRef for any input that is not
// `owner@epoch` with a non-empty owner and a non-negative decimal epoch.
var ErrMalformedHoldRef = errors.New("malformed hold ref, want owner@epoch")

// HoldRef names ONE hold — a single grant of a target to an owner — and not
// merely the owner. The distinction is the whole point: an owner UUID is
// stable across release-and-reacquire, so a break authorized against `alice`
// would still land after alice let go and took the path again, on a hold the
// caller never read. The generation qualifier closes that.
//
// ‡ Why Epoch and not CreatedAt. Both distinguish generations; only Epoch
// distinguishes the RIGHT ones. path_epochs counts the AUTHORIZATION to write
// a path (epoch.go): a renewal by a live owner PRESERVES it, while every fresh
// grant after release, stale reclaim or forced break bumps it. So a HoldRef
// captured before a lease refresh still matches afterwards — the hold did not
// change, only its TTL — where a created_at-keyed ref would spuriously
// mismatch and refuse a legitimate break. Epoch is also small enough to read
// off `loto status` and retype; created_at is a unix-nanosecond integer nobody
// transcribes correctly.
//
// A target's holder set is HoldRefs, plural: shared mode lets several owners
// coexist on one path (I1), so "the holder" is a set, not a value.
type HoldRef struct {
	Owner AgentUUID
	// Epoch is the path-epoch the hold was granted at. Zero is meaningful, not
	// absent: rows predating the epoch column read as 0, which is also the
	// correct reading of "nothing ever captured a generation against this".
	Epoch int64
}

// String renders the wire/CLI form, `owner@epoch`. Stable — `loto status`
// prints the two halves and a caller reassembles them for --expect-holder.
func (h HoldRef) String() string {
	return string(h.Owner) + "@" + strconv.FormatInt(h.Epoch, 10)
}

// ParseHoldRef reads the `owner@epoch` CLI form. It splits on the LAST '@' so
// an owner UUID that itself contains one still round-trips through String.
func ParseHoldRef(s string) (HoldRef, error) {
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 {
		return HoldRef{}, fmt.Errorf("%w: %q", ErrMalformedHoldRef, s)
	}
	epoch, err := strconv.ParseInt(s[at+1:], 10, 64)
	if err != nil || epoch < 0 {
		return HoldRef{}, fmt.Errorf("%w: %q", ErrMalformedHoldRef, s)
	}
	return HoldRef{Owner: AgentUUID(s[:at]), Epoch: epoch}, nil
}

// SortHoldRefs orders a hold set by owner then epoch, in place. Both sides of
// a compare-and-swap normalize through it, so the comparison is set-shaped
// (order-independent) while the reporting stays deterministic.
func SortHoldRefs(refs []HoldRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Owner != refs[j].Owner {
			return refs[i].Owner < refs[j].Owner
		}
		return refs[i].Epoch < refs[j].Epoch
	})
}

// HoldRefsEqual reports whether two hold sets name exactly the same holds.
// Both inputs must already be sorted (SortHoldRefs). Equality is EXACT, not
// subset: a holder that joined a shared target since the caller read it is one
// the caller never authorized breaking, so its arrival must fail the compare.
func HoldRefsEqual(a, b []HoldRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// FormatHoldRefs joins a sorted hold set for a one-line report, or "none" when
// empty — an empty set is a real outcome (the hold vanished between the read
// and the break) and must not render as blank, which reads as a missing field.
func FormatHoldRefs(refs []HoldRef) string {
	if len(refs) == 0 {
		return "none"
	}
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = r.String()
	}
	return strings.Join(parts, ",")
}
