package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2E_MailLifecycle walks the loto-qhw mailbox scenario: alice mails bob
// by handle → bob's status surfaces the unread banner → bob reads and marks →
// banner gone. Then @all and @<slug> routing, with per-reader read cursors.
func TestE2E_MailLifecycle(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)

	asAlice := func() { t.Setenv("LOTO_AGENT_ID", alice.UUID) }
	asBob := func() { t.Setenv("LOTO_AGENT_ID", bob.UUID) }
	run := func(argv ...string) (string, string, int) {
		var out, errBuf bytes.Buffer
		code := Run(argv, &out, &errBuf)
		return out.String(), errBuf.String(), code
	}

	// 1. alice mails bob by handle.
	asAlice()
	out, errOut, code := run(tcMsg, bob.Handle, "-t", "loto-qhw: ping bob")
	if code != 0 {
		t.Fatalf("alice msg: code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, "✓ sent id=m-") {
		t.Fatalf("send confirmation missing: %q", out)
	}

	// 2. bob's status carries the unread banner (ambient delivery).
	asBob()
	out, _, code = run("status")
	if code != 0 {
		t.Fatalf("bob status: code=%d", code)
	}
	if !strings.Contains(out, "ℹ mail unread=1") {
		t.Fatalf("status should carry mail banner: %q", out)
	}

	// 3. bob reads: sees the body, marks read.
	out, _, code = run("inbox", "--mark-read")
	if code != 0 {
		t.Fatalf("bob inbox: code=%d", code)
	}
	if !strings.Contains(out, "loto-qhw: ping bob") || !strings.Contains(out, "✓ marked read count=1") {
		t.Fatalf("inbox read/mark output wrong: %q", out)
	}

	// 4. banner and inbox now clean; --summary is silent.
	out, _, _ = run("status")
	if strings.Contains(out, "mail unread") {
		t.Fatalf("banner should clear after mark-read: %q", out)
	}
	out, _, code = run("inbox", "--summary")
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("summary should be silent when empty: code=%d out=%q", code, out)
	}

	// 5. alice sends @all — bob sees it, alice doesn't see her own broadcast.
	asAlice()
	if _, _, code := run(tcMsg, "@all", "-t", "loto-qhw: broadcast"); code != 0 {
		t.Fatalf("msg @all: code=%d", code)
	}
	out, _, _ = run("inbox")
	if !strings.Contains(out, "✓ inbox empty") {
		t.Fatalf("sender must not see own broadcast: %q", out)
	}
	asBob()
	out, _, _ = run("inbox")
	if !strings.Contains(out, "loto-qhw: broadcast") {
		t.Fatalf("bob should see @all: %q", out)
	}

	// 6. @<slug> routing: the temp repo's slug is test-proj (from the fake
	//    origin remote); @elsewhere mail must not reach bob here.
	asAlice()
	if _, _, code := run(tcMsg, "@test-proj", "-t", "loto-qhw: repo mail"); code != 0 {
		t.Fatalf("msg @test-proj: code=%d", code)
	}
	if _, _, code := run(tcMsg, "@elsewhere", "-t", "loto-qhw: other repo"); code != 0 {
		t.Fatalf("msg @elsewhere: code=%d", code)
	}
	asBob()
	out, _, _ = run("inbox")
	if !strings.Contains(out, "repo mail") {
		t.Fatalf("bob should see @test-proj mail: %q", out)
	}
	if strings.Contains(out, "other repo") {
		t.Fatalf("bob must not see @elsewhere mail: %q", out)
	}

	// 7. dir-basename alias: the pinned slug is test-proj (remote-derived) but
	//    a sender who only knows the checkout dir addresses its basename —
	//    both must deliver (loto-ykp: @ferret vs @dkoosis-ferret dead-letter).
	base := "@" + filepath.Base(repo)
	asAlice()
	if _, _, code := run(tcMsg, base, "-t", "loto-ykp: dir-alias mail"); code != 0 {
		t.Fatalf("msg %s: code=%d", base, code)
	}
	asBob()
	out, _, _ = run("inbox")
	if !strings.Contains(out, "dir-alias mail") {
		t.Fatalf("bob should see %s mail via basename alias: %q", base, out)
	}
}

func TestMsgRejectsUnknownHandleAndEmptyBody(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	var out, errBuf bytes.Buffer
	if code := Run([]string{tcMsg, "NoSuchAgent", "-t", "x: hi"}, &out, &errBuf); code != 2 {
		t.Fatalf("unknown handle should exit 2, got %d (err=%q)", code, errBuf.String())
	}
	errBuf.Reset()
	if code := Run([]string{tcMsg, "@loto"}, &out, &errBuf); code != 2 {
		t.Fatalf("missing -t should exit 2, got %d", code)
	}
	if !strings.Contains(errBuf.String(), "usage: loto msg") {
		t.Fatalf("usage teaching surface missing: %q", errBuf.String())
	}
	errBuf.Reset()
	if code := Run([]string{tcMsg, "@all", "-t", "x: hi", tcFlagTTL, "-1h"}, &out, &errBuf); code != 2 {
		t.Fatalf("negative ttl should exit 2, got %d", code)
	}
	if !strings.Contains(errBuf.String(), "--ttl must be non-negative") {
		t.Fatalf("negative-ttl rejection message missing: %q", errBuf.String())
	}
}
