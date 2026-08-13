package identity

import (
	"errors"
	"testing"
)

// errNoHostname stands in for whatever the OS returns when the lookup fails —
// static so err113 stays satisfied.
var errNoHostname = errors.New("hostname lookup failed")

func TestHostIDReportsUnknownWhenHostnameFails(t *testing.T) {
	t.Setenv("LOTO_HOST", "")
	swapHostname(t, func() (string, error) { return "", errNoHostname })

	got, ok := HostID()
	if ok {
		t.Errorf("HostID() ok = true, want false when os.Hostname fails")
	}
	if got != "" {
		t.Errorf("HostID() = %q, want empty when os.Hostname fails", got)
	}
}

func TestHostIDReportsUnknownOnEmptyHostname(t *testing.T) {
	t.Setenv("LOTO_HOST", "")
	swapHostname(t, func() (string, error) { return "", nil })

	if got, ok := HostID(); ok || got != "" {
		t.Errorf("HostID() = (%q, %v), want (\"\", false) for an empty hostname", got, ok)
	}
}

// The env leg is what keeps pid-based stale-lock reclaim alive on a box whose
// hostname lookup is broken, so it must never consult the OS.
func TestHostIDPrefersLotoHostOverFailedLookup(t *testing.T) {
	t.Setenv("LOTO_HOST", "  ci-box  ")
	swapHostname(t, func() (string, error) { return "", errNoHostname })

	got, ok := HostID()
	if !ok {
		t.Errorf("HostID() ok = false, want true when LOTO_HOST is set")
	}
	if got != "ci-box" {
		t.Errorf("HostID() = %q, want %q (trimmed)", got, "ci-box")
	}
}

func TestHostIDIgnoresBlankLotoHost(t *testing.T) {
	t.Setenv("LOTO_HOST", "   ")
	swapHostname(t, func() (string, error) { return "real-box", nil })

	if got, ok := HostID(); !ok || got != "real-box" {
		t.Errorf("HostID() = (%q, %v), want (%q, true) — blank LOTO_HOST must fall through", got, ok, "real-box")
	}
}

func swapHostname(t *testing.T, fn func() (string, error)) {
	t.Helper()
	prev := osHostname
	osHostname = fn
	t.Cleanup(func() { osHostname = prev })
}
