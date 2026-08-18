package gate

import (
	"crypto/rand"
	"encoding/hex"
)

// NewCandidateID returns an opaque unique candidate identity: an 8-random-byte
// hex string, prefixed "c-" so it reads unambiguously beside a bead id or a
// SHA in a log line. Mirrors internal/store/event_ids.go's newID exactly —
// same shape, same crypto/rand-failure-is-a-panic reasoning — but declared
// here rather than imported: gate stays store-independent (envelope.go's own
// invariant), and a candidate's identity is a gate-owned concept regardless of
// which store, if any, ends up persisting claims against it.
func NewCandidateID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return "c-" + hex.EncodeToString(b[:])
}
