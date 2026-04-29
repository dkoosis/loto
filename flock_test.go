//go:build unix

package loto

import (
	"errors"
	"syscall"
	"testing"
)

// wrapFlockErr must map only EWOULDBLOCK/EAGAIN to errFlockContention.
// Any other syscall error indicates a real failure (EIO, EBADF, ENOLCK,
// disk fault, etc.) and must propagate untouched so callers return
// ErrSystem rather than misreporting "held by X" — which would trigger
// retry loops on hardware/filesystem outages and mislead operators.
func TestWrapFlockErrDiscriminatesContention(t *testing.T) {
	tests := []struct {
		name           string
		in             error
		wantContention bool
		wantPassthru   bool // err must be returned unchanged
	}{
		{"nil", nil, false, false},
		{"ewouldblock", syscall.EWOULDBLOCK, true, false},
		{"eagain", syscall.EAGAIN, true, false},
		{"eio", syscall.EIO, false, true},
		{"ebadf", syscall.EBADF, false, true},
		{"enolck", syscall.ENOLCK, false, true},
		{"enospc", syscall.ENOSPC, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapFlockErr(tc.in)
			if tc.in == nil {
				if got != nil {
					t.Fatalf("nil in → %v", got)
				}
				return
			}
			if isFlockContention(got) != tc.wantContention {
				t.Errorf("isFlockContention(%v) = %v, want %v", tc.in, isFlockContention(got), tc.wantContention)
			}
			if tc.wantPassthru && !errors.Is(got, tc.in) {
				t.Errorf("expected passthru of %v, got %v", tc.in, got)
			}
			if tc.wantContention && errors.Is(got, tc.in) {
				t.Errorf("contention errors must NOT expose original errno; got %v wraps %v", got, tc.in)
			}
		})
	}
}
