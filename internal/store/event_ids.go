package store

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

func newEventID(actorUUID string, t time.Time, reason string) string {
	h := sha256.New()
	h.Write([]byte(actorUUID))
	var buf [8]byte
	ns := t.UnixNano()
	for i := 7; i >= 0; i-- {
		buf[i] = byte(ns)
		ns >>= 8
	}
	h.Write(buf[:])
	h.Write([]byte(reason))
	sum := h.Sum(nil)
	return "e-" + hex.EncodeToString(sum[:4])
}
