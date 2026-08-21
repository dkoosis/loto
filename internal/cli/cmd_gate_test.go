package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	tcCmdGate = "gate"
	tcSubStat = "stats"
)

func runGateStats(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := Run(append([]string{tcCmdGate, tcSubStat}, args...), &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// A fresh repo reports every class at zero rather than printing nothing:
// which classes have NEVER fired is the reading the report exists for.
func TestGateStats_FreshRepoEnumeratesEveryClassAtZero(t *testing.T) {
	violationRepo(t)

	code, out, errOut := runGateStats(t)
	if code != 0 {
		t.Fatalf("exit=%d err=%q", code, errOut)
	}
	if !strings.Contains(out, "✓ gate-stats since=24h0m0s judged=0 accepted=0 rejected=0 bypassed=0") {
		t.Errorf("headline wrong: %q", out)
	}
	for _, class := range []string{
		"violation-intersect", "stale-preimage", "unauthorized-path", "gate-bypass",
		"verify-red", "verify-infrastructure", "promotion-race",
	} {
		if !strings.Contains(out, "ℹ class="+class+" count=0") {
			t.Errorf("missing zero row for %s: %q", class, out)
		}
	}
}

// End to end: a real rejection, counted under its real class.
func TestGateStats_CountsARealViolationRejection(t *testing.T) {
	repo := violationRepo(t)
	rogueWrite(t, repo, tcTargetB, "rogue\n")
	if code, out := runViolations(t, tcSubScan); code != 1 {
		t.Fatalf("seed scan: exit=%d out=%q", code, out)
	}
	if code := Run([]string{tcCmdLock, tcTargetB, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock")
	}
	if code := Run([]string{tcCmdSubmit, tcTargetB, tcFlagBead, tcBeadOvno9}, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Fatalf("want the submit refused, got %d", code)
	}

	_, out, _ := runGateStats(t)
	if !strings.Contains(out, "✗ class=violation-intersect count=1") {
		t.Errorf("the rejection was not counted: %q", out)
	}
	if !strings.Contains(out, "judged=1 accepted=0 rejected=1") {
		t.Errorf("headline wrong: %q", out)
	}
}

// An accepted candidate is counted too — a report that only shows failures
// cannot say what fraction of candidates the gate lets through.
func TestGateStats_CountsAnAcceptance(t *testing.T) {
	repo := violationRepo(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock")
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sOut, sErr bytes.Buffer
	if code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno9}, &sOut, &sErr); code != 0 {
		t.Fatalf("submit: exit=%d out=%q err=%q", code, sOut.String(), sErr.String())
	}

	_, out, _ := runGateStats(t)
	if !strings.Contains(out, "judged=1 accepted=1 rejected=0") {
		t.Errorf("the acceptance was not counted: %q", out)
	}
}

// LOTO_GATE=off must show up as a bypass, never as an acceptance: a
// bypassed candidate was never judged, and the report is the one place the
// escape hatch cannot be allowed to hide.
func TestGateStats_BypassIsNotReportedAsAnAcceptance(t *testing.T) {
	repo := violationRepo(t)
	t.Setenv("LOTO_GATE", "off")
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock")
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sOut, sErr bytes.Buffer
	if code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno9}, &sOut, &sErr); code != 0 {
		t.Fatalf("submit under bypass: exit=%d err=%q", code, sErr.String())
	}

	_, out, _ := runGateStats(t)
	if !strings.Contains(out, "judged=0 accepted=0 rejected=0 bypassed=1") {
		t.Errorf("bypass mis-counted: %q", out)
	}
	if !strings.Contains(out, "✗ class=gate-bypass count=1") {
		t.Errorf("missing gate-bypass class row: %q", out)
	}
}

func TestGateStats_RejectsANonPositiveWindow(t *testing.T) {
	violationRepo(t)

	code, _, errOut := runGateStats(t, "--since", "0s")
	if code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(errOut, "--since must be positive") {
		t.Errorf("missing refusal row: %q", errOut)
	}
}

// `loto gate` with no subcommand teaches rather than failing silently.
func TestGate_UnknownSubcommandPrintsUsage(t *testing.T) {
	violationRepo(t)

	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdGate}, &out, &errBuf); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(errBuf.String(), "usage: loto gate stats") {
		t.Errorf("missing usage: %q", errBuf.String())
	}
}
