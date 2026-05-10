package store

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

func newTagID(authorUUID string, t time.Time, intent string) string {
	h := sha256.New()
	h.Write([]byte(authorUUID))
	var buf [8]byte
	ns := t.UnixNano()
	for i := 7; i >= 0; i-- {
		buf[i] = byte(ns)
		ns >>= 8
	}
	h.Write(buf[:])
	h.Write([]byte(intent))
	sum := h.Sum(nil)
	return "t-" + hex.EncodeToString(sum[:4])
}
