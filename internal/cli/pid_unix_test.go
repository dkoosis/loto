//go:build unix

package cli

import (
	"syscall"
	"testing"
)

func TestPidLive(t *testing.T) {
	orig := killFn
	t.Cleanup(func() { killFn = orig })

	tests := []struct {
		name string
		pid  int
		err  error
		want bool
	}{
		{"zero pid is dead", 0, nil, false},
		{"negative pid is dead", -1, nil, false},
		{"kill ok means live", 1234, nil, true},
		{"EPERM means foreign-uid live", 1234, syscall.EPERM, true},
		{"ESRCH means dead", 1234, syscall.ESRCH, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			killFn = func(pid int, sig syscall.Signal) error { return tc.err }
			if got := pidLive(tc.pid); got != tc.want {
				t.Fatalf("pidLive(%d) err=%v: got %v want %v", tc.pid, tc.err, got, tc.want)
			}
		})
	}
}
