//go:build linux || darwin

package identity

import (
	"os"
	"testing"
)

func TestProcStart(t *testing.T) {
	t.Run("self is stable and nonzero", func(t *testing.T) {
		s1, ok1 := ProcStart(os.Getpid())
		s2, ok2 := ProcStart(os.Getpid())
		if !ok1 || !ok2 {
			t.Fatalf("ProcStart(self) ok = %v/%v, want true (reader unavailable on this OS?)", ok1, ok2)
		}
		if s1 == 0 {
			t.Fatal("ProcStart(self) returned 0; expected nonzero start-time")
		}
		if s1 != s2 {
			t.Fatalf("ProcStart(self) not stable: %d != %d", s1, s2)
		}
	})

	t.Run("never-existed pid is unknown", func(t *testing.T) {
		// PID 0 / negative can never be a real user process to read start-time
		// for; the reader must report unknown rather than a bogus value.
		if s, ok := ProcStart(-1); ok {
			t.Fatalf("ProcStart(-1) = (%d,true), want unknown", s)
		}
	})
}
