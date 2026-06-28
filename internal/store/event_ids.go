package store

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns an opaque unique ID: prefix + 8 random hex-encoded bytes.
// crypto/rand failing on a working OS is catastrophic and not recoverable by
// the caller — propagate as a panic rather than silently mint a predictable ID.
func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return prefix + hex.EncodeToString(b[:])
}

// newEventID returns a unique opaque event ID. We previously derived the ID
// deterministically from (actor || ns || reason), but that collides on the
// same-actor-same-instant case — DoctorRepair reclaiming two stale locks owned
// by the same uuid in one transaction hit UNIQUE constraint failed: events.id
// and rolled back the repair (audit findings xoy, ri4). 8 random bytes also
// retires the 32-bit birthday floor (yy7); reader treats the string as opaque.
func newEventID() string { return newID("e-") }
