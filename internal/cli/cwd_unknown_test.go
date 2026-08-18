package cli

import (
	"bytes"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"os"
)

const tcFlagCwdUnknown = "--cwd-unknown"

// TestCheckCwdUnknown_RefusesRelativePath reproduces loto-tzmv.10 directly: a
// peer holds internal/store/store.go, and a relative token that names it from
// some other directory used to resolve against the repo root and print
// "✓ no conflicts", exit 0 — a false CLEAN, which reads as protection. Under
// --cwd-unknown the same token is refused instead of guessed.
func TestCheckCwdUnknown_RefusesRelativePath(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)

	// "store.go" is the peer-held file named from inside internal/store. From
	// the repo root it resolves to a different (unheld) path entirely.
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagCwdUnknown, "store.go"}, &out, &bytes.Buffer{})
	if code == 0 {
		t.Fatalf("a relative path from a cwd-unknown caller must never yield a clean verdict; got exit 0: %q", out.String())
	}
	if !strings.Contains(out.String(), "✗ unresolvable count=1") {
		t.Errorf("expected the unresolvable refusal row: %q", out.String())
	}
	if !strings.Contains(out.String(), "reason=relative-path-caller-cwd-unknown") {
		t.Errorf("expected the reason on the path row: %q", out.String())
	}
	if strings.Contains(out.String(), "no conflicts") {
		t.Errorf("refusal must not carry a clean verdict: %q", out.String())
	}
}

// TestCheckCwdUnknown_AbsolutePathStillWorks: absolute paths carry their own
// base, so --cwd-unknown must not touch them. Pins the "absolute paths keep
// working as today" half of the AC.
func TestCheckCwdUnknown_AbsolutePathStillWorks(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)

	abs := filepath.Join(repo, tcTargetA)
	var out bytes.Buffer
	if code := Run([]string{tcCmdCheck, tcFlagCwdUnknown, abs}, &out, &bytes.Buffer{}); code != 1 {
		t.Fatalf("expected the normal conflict verdict for an absolute path, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ conflicts") {
		t.Errorf("expected conflict report: %q", out.String())
	}
	if strings.Contains(out.String(), "unresolvable") {
		t.Errorf("absolute path must not be refused: %q", out.String())
	}
}

// TestCheck_RelativePathWithoutFlagUnchanged: the refusal is opt-in per call
// site. Bash resolves relative paths soundly (fresh process per call, the
// payload cwd plus any leading cd), so its behavior must be byte-identical to
// before — the per-tool difference is the point of the flag.
func TestCheck_RelativePathWithoutFlagUnchanged(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	var out bytes.Buffer
	if code := Run([]string{tcCmdCheck, tcTargetA}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("expected exit 0 without the flag, got %d: %q", code, out.String())
	}
	if strings.Contains(out.String(), "unresolvable") {
		t.Errorf("plain check must not refuse relative paths: %q", out.String())
	}
}

// TestCheckGate_CwdUnknownRefusesRelativePath: the gate surface carries the
// same rule. The gate is the one the hook actually calls, and a false clean
// there is what waved the reproduced `mv` through.
func TestCheckGate_CwdUnknownRefusesRelativePath(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcFlagCwdUnknown, "store.go"}, &out, &bytes.Buffer{})
	if code == 0 {
		t.Fatalf("gate must not return clean for an unresolvable relative path; got exit 0: %q", out.String())
	}
	if !strings.Contains(out.String(), "✗ unresolvable count=1") {
		t.Errorf("expected the unresolvable refusal row: %q", out.String())
	}
}
