package cli

import (
	"strings"
	"testing"
)

// TestStampPID pins the durability contract: only a valid LOTO_PID yields a
// durable liveness pid (pidDurable). Unset → pidUnset, set-but-bad → pidInvalid,
// both with the PID-0 sentinel (loto-j1bo), so the one-shot CLI's own dying pid
// is never stamped as a holder's liveness token. The distinct sources let the
// degrade warning name the right cause without re-reading the env.
func TestStampPID(t *testing.T) {
	t.Run("valid LOTO_PID is durable and returned verbatim", func(t *testing.T) {
		t.Setenv("LOTO_PID", "4242")
		pid, src := stampPID()
		if pid != 4242 || src != pidDurable {
			t.Fatalf("got (%d,%v), want (4242,pidDurable)", pid, src)
		}
	})
	t.Run("empty LOTO_PID degrades to sentinel 0, pidUnset", func(t *testing.T) {
		t.Setenv("LOTO_PID", "")
		pid, src := stampPID()
		if pid != 0 || src != pidUnset {
			t.Fatalf("got (%d,%v), want (0,pidUnset)", pid, src)
		}
	})
	t.Run("non-numeric LOTO_PID degrades to sentinel 0, pidInvalid", func(t *testing.T) {
		t.Setenv("LOTO_PID", "notanint")
		pid, src := stampPID()
		if pid != 0 || src != pidInvalid {
			t.Fatalf("got (%d,%v), want (0,pidInvalid)", pid, src)
		}
	})
	t.Run("non-positive LOTO_PID degrades to sentinel 0, pidInvalid", func(t *testing.T) {
		t.Setenv("LOTO_PID", "0")
		pid, src := stampPID()
		if pid != 0 || src != pidInvalid {
			t.Fatalf("got (%d,%v), want (0,pidInvalid)", pid, src)
		}
	})
}

// TestDegradedPidWarning pins the one-line stderr notice: it fires only inside a
// detectable Claude session (LOTO_AGENT_ID set) that is missing LOTO_PID, so the
// misconfigured hook is surfaced — but bare shells degrade silently (expected
// there) and a durable pid says nothing.
func TestDegradedPidWarning(t *testing.T) {
	t.Run("Claude session with unset LOTO_PID warns 'unset'", func(t *testing.T) {
		t.Setenv("LOTO_AGENT_ID", "00000000-0000-4000-8000-000000000000")
		t.Setenv("LOTO_PID", "")
		w := degradedPidWarning()
		if !strings.Contains(w, "unset") {
			t.Fatalf("want an 'unset' warning, got %q", w)
		}
	})
	t.Run("Claude session with invalid LOTO_PID warns 'invalid', not 'unset'", func(t *testing.T) {
		t.Setenv("LOTO_AGENT_ID", "00000000-0000-4000-8000-000000000000")
		t.Setenv("LOTO_PID", "notanint")
		w := degradedPidWarning()
		if !strings.Contains(w, "invalid") || strings.Contains(w, "unset") {
			t.Fatalf("want an 'invalid' warning (not 'unset'), got %q", w)
		}
	})
	t.Run("durable LOTO_PID is silent", func(t *testing.T) {
		t.Setenv("LOTO_AGENT_ID", "00000000-0000-4000-8000-000000000000")
		t.Setenv("LOTO_PID", "4242")
		if w := degradedPidWarning(); w != "" {
			t.Fatalf("durable pid must not warn, got %q", w)
		}
	})
	t.Run("bare shell (no LOTO_AGENT_ID) degrades silently", func(t *testing.T) {
		t.Setenv("LOTO_AGENT_ID", "")
		t.Setenv("LOTO_PID", "")
		if w := degradedPidWarning(); w != "" {
			t.Fatalf("bare shell must degrade silently, got %q", w)
		}
	})
}
