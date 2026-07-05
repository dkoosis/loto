package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const (
	tcCmdClaim      = "claim"
	tcCmdUnclaim    = "unclaim"
	tcPrefixStore   = "internal/store"
	tcPrefixParent  = "internal"
	tcPrefixNoDisk  = "pkg/notyet"
	tcClaimNotOwner = "state=not-owner"
)

func TestClaimCmdUsageErrors(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	cases := []struct {
		name string
		args []string
	}{
		{"no-args", []string{tcCmdClaim, "-t", tcIntentTest}},
		{"missing-intent", []string{tcCmdClaim, tcPrefixStore}},
		{"two-prefixes", []string{tcCmdClaim, tcPrefixStore, tcPrefixParent, "-t", tcIntentTest}},
		{"glob", []string{tcCmdClaim, "internal/*", "-t", tcIntentTest}},
		{"repo-root", []string{tcCmdClaim, ".", "-t", tcIntentTest}},
		{"unclaim-no-args", []string{tcCmdUnclaim}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			if code := Run(c.args, &out, &errBuf); code != 2 {
				t.Errorf("exit=%d; want 2; out=%q err=%q", code, out.String(), errBuf.String())
			}
		})
	}
}

func TestClaimCmdAcquireUnclaimTTL(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	// Acquire: dir exists on disk → no advisory.
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest}, &out, &errBuf); code != 0 {
		t.Fatalf("claim exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ claimed count=1") || !strings.Contains(out.String(), "prefix=internal/store") {
		t.Errorf("claim output: %q", out.String())
	}
	if strings.Contains(out.String(), "⚠") {
		t.Errorf("on-disk prefix must not warn: %q", out.String())
	}

	// Same-owner re-claim refreshes (trailing slash spelling normalizes too).
	out.Reset()
	if code := Run([]string{tcCmdClaim, "internal/store/", "-t", "refresh"}, &out, &errBuf); code != 0 {
		t.Fatalf("refresh exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}

	// Not-on-disk prefix: ⚠ advisory row, claim still lands (exit 0).
	out.Reset()
	if code := Run([]string{tcCmdClaim, tcPrefixNoDisk, "-t", tcIntentTest}, &out, &errBuf); code != 0 {
		t.Fatalf("not-on-disk claim exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "⚠ prefix=pkg/notyet not-on-disk") {
		t.Errorf("expected not-on-disk advisory: %q", out.String())
	}
	if !strings.Contains(out.String(), "✓ claimed count=1") {
		t.Errorf("advisory must not block the claim: %q", out.String())
	}

	// Unclaim by owner.
	out.Reset()
	if code := Run([]string{tcCmdUnclaim, tcPrefixStore}, &out, &errBuf); code != 0 {
		t.Fatalf("unclaim exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ unclaimed count=1") {
		t.Errorf("unclaim output: %q", out.String())
	}

	// Unclaim again: no claim → ℹ row, exit 0.
	out.Reset()
	if code := Run([]string{tcCmdUnclaim, tcPrefixStore}, &out, &errBuf); code != 0 {
		t.Fatalf("unclaim-none exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "state=no-claim") {
		t.Errorf("expected no-claim row: %q", out.String())
	}

	// TTL e2e: expired claim is reclaimed by a second agent. Generous margin —
	// ttl≈1ms then wait ≥50ms (flake guard, loto-claim plan P-self #3).
	out.Reset()
	if code := Run([]string{tcCmdClaim, "pkg/ttl", "-t", tcIntentTest, tcFlagTTL, "1ms"}, &out, &errBuf); code != 0 {
		t.Fatalf("short-ttl claim exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	time.Sleep(60 * time.Millisecond)
	pinAgent(t) // second identity
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{tcCmdClaim, "pkg/ttl", "-t", tcIntentTest}, &out, &errBuf); code != 0 {
		t.Fatalf("expired claim must be reclaimable: exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
}

func TestClaimCmdBlockedNamesHolder(t *testing.T) {
	withTempProject(t)
	pinAgent(t) // agent A
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("agent A claim failed")
	}
	pinAgent(t) // agent B

	// Overlap via parent prefix → blocked, holder named.
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdClaim, tcPrefixParent, "-t", tcIntentTest}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit=%d; want 1; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✗ blocked count=1") || !strings.Contains(out.String(), "blocker=") {
		t.Errorf("blocked output must name holder: %q", out.String())
	}

	// B unclaiming A's prefix → not-owner, exit 1, names actual owner.
	out.Reset()
	code = Run([]string{tcCmdUnclaim, tcPrefixStore}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("not-owner unclaim exit=%d; want 1; out=%q", code, out.String())
	}
	if !strings.Contains(out.String(), tcClaimNotOwner) || !strings.Contains(out.String(), "owner=") {
		t.Errorf("not-owner row must name owner: %q", out.String())
	}
}
