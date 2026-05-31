package cli

import "testing"

// TestStampPID pins the durability contract: only a valid LOTO_PID yields a
// durable liveness pid. Absent/invalid → the PID-0 sentinel (loto-j1bo), so the
// one-shot CLI's own dying pid is never stamped as a holder's liveness token.
func TestStampPID(t *testing.T) {
	t.Run("valid LOTO_PID is durable and returned verbatim", func(t *testing.T) {
		t.Setenv("LOTO_PID", "4242")
		pid, durable := stampPID()
		if pid != 4242 || !durable {
			t.Fatalf("got (%d,%v), want (4242,true)", pid, durable)
		}
	})
	t.Run("empty LOTO_PID degrades to sentinel 0, not durable", func(t *testing.T) {
		t.Setenv("LOTO_PID", "")
		pid, durable := stampPID()
		if pid != 0 || durable {
			t.Fatalf("got (%d,%v), want (0,false)", pid, durable)
		}
	})
	t.Run("non-numeric LOTO_PID degrades to sentinel 0, not durable", func(t *testing.T) {
		t.Setenv("LOTO_PID", "notanint")
		pid, durable := stampPID()
		if pid != 0 || durable {
			t.Fatalf("got (%d,%v), want (0,false)", pid, durable)
		}
	})
	t.Run("non-positive LOTO_PID degrades to sentinel 0, not durable", func(t *testing.T) {
		t.Setenv("LOTO_PID", "0")
		pid, durable := stampPID()
		if pid != 0 || durable {
			t.Fatalf("got (%d,%v), want (0,false)", pid, durable)
		}
	})
}

// TestDegradedPidWarning pins the one-line stderr notice: it fires only inside a
// detectable Claude session (LOTO_AGENT_ID set) that is missing LOTO_PID, so the
// misconfigured hook is surfaced — but bare shells degrade silently (expected
// there) and a durable pid says nothing.
func TestDegradedPidWarning(t *testing.T) {
	t.Run("Claude session without LOTO_PID warns", func(t *testing.T) {
		t.Setenv("LOTO_AGENT_ID", "00000000-0000-4000-8000-000000000000")
		t.Setenv("LOTO_PID", "")
		if degradedPidWarning() == "" {
			t.Fatal("expected a degrade warning when LOTO_AGENT_ID set but LOTO_PID unset")
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
