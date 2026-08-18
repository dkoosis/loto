package gate

import "os"

// Bypassed reports whether LOTO_GATE=off is set — the escape hatch for "the
// day the gate itself is broken" (git-gate.md Outcome 7: "The gate can never
// become the outage"). A caller checking this before calling Admit must, on
// true, both print the loud advisory (BypassAdvisory) and record the event
// (a store-layer concern — s.RecordGateBypass, internal/store/admission.go)
// so a bypass can never silently become the norm.
//
// Deliberately exact-match "off", not a truthy-string parse: an env var
// typo'd to "1" or "true" must NOT bypass the gate by accident — the escape
// hatch is for a human who typed LOTO_GATE=off on purpose.
func Bypassed() bool {
	return os.Getenv("LOTO_GATE") == "off"
}

// BypassAdvisory is the loud, unmissable warning a caller prints before
// skipping admission. Returned as a string rather than written directly to an
// io.Writer — this package has no opinion on stdout vs stderr, matching every
// other gate function's stance of doing exactly one job.
const BypassAdvisory = "⚠ LOTO_GATE=off — admission bypassed. This candidate is NOT verified. Every bypass is logged." //nolint:gosec // G101: false positive — an advisory message, not a credential; nothing here is secret
