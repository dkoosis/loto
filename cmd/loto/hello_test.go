//go:build unix

package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"loto"
)

const (
	subcmdHello    = "hello"
	flagIntentLong = "--intent"
	helloTestGlob  = "internal/store/**"
)

// TestHello_ReserveOnly: --to absent → reservation present, no msgs sent, exit 0.
func TestHello_ReserveOnly(t *testing.T) {
	base := t.TempDir()
	glob := helloTestGlob

	out, err := lotoCmd(base, flagAgentLong, "Scout",
		subcmdHello, glob, flagIntentLong, "loto-7wp.4 store refactor").Output()
	if err != nil {
		t.Fatalf("hello: %v\n%s", err, out)
	}

	// Reservation should be present.
	listOut, err := lotoCmd(base, "reserve", "list").Output()
	if err != nil {
		t.Fatalf("reserve list: %v\n%s", err, listOut)
	}
	if !strings.Contains(string(listOut), glob) {
		t.Fatalf("reservation not added: %s", listOut)
	}

	// No "to" recipients in output.
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse hello output: %v\n%s", err, out)
	}
	if r, ok := result["to"].([]any); ok && len(r) > 0 {
		t.Errorf("expected no recipients, got %v", r)
	}
}

// TestHello_SendsStructuredBody: single sibling receives a templated msg whose
// body contains the stable structured fields.
func TestHello_SendsStructuredBody(t *testing.T) {
	base := t.TempDir()
	glob := helloTestGlob

	out, err := lotoCmd(base, flagAgentLong, "Scout",
		subcmdHello, glob, flagIntentLong, "store refactor",
		flagToLong, "GreenCastle").Output()
	if err != nil {
		t.Fatalf("hello: %v\n%s", err, out)
	}
	_ = out

	// Read GreenCastle's mailbox at the glob target.
	l, err := loto.New(base)
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(glob)
	msgs, err := l.ReadMsgs(abs, "GreenCastle")
	if err != nil {
		t.Fatalf("read msgs: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(msgs))
	}
	body := msgs[0].Body
	for _, want := range []string{
		"loto:llm:v1 hello",
		"handle:Scout",
		"glob:" + glob,
		"intent:store refactor",
		"tiebreaker:msg+switch>2min",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody: %s", want, body)
		}
	}
}

// TestHello_NoTiebreaker: --no-tiebreaker omits the tiebreaker field.
func TestHello_NoTiebreaker(t *testing.T) {
	base := t.TempDir()
	glob := helloTestGlob

	if out, err := lotoCmd(base, flagAgentLong, "Scout",
		subcmdHello, glob, flagIntentLong, "x",
		flagToLong, "GreenCastle", "--no-tiebreaker").Output(); err != nil {
		t.Fatalf("hello: %v\n%s", err, out)
	}

	l, err := loto.New(base)
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(glob)
	msgs, err := l.ReadMsgs(abs, "GreenCastle")
	if err != nil {
		t.Fatalf("read msgs: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(msgs))
	}
	if strings.Contains(msgs[0].Body, "tiebreaker:") {
		t.Errorf("tiebreaker should be omitted: %s", msgs[0].Body)
	}
}

// TestHello_MultiSibling: comma-separated --to delivers to each.
func TestHello_MultiSibling(t *testing.T) {
	base := t.TempDir()
	glob := helloTestGlob

	if out, err := lotoCmd(base, flagAgentLong, "Scout",
		subcmdHello, glob, flagIntentLong, "x",
		flagToLong, "GreenCastle,BlueOak").Output(); err != nil {
		t.Fatalf("hello: %v\n%s", err, out)
	}

	l, err := loto.New(base)
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(glob)
	for _, who := range []string{"GreenCastle", "BlueOak"} {
		msgs, err := l.ReadMsgs(abs, who)
		if err != nil {
			t.Fatalf("read msgs %s: %v", who, err)
		}
		if len(msgs) != 1 {
			t.Errorf("%s: expected 1 msg, got %d", who, len(msgs))
		}
	}
}

// TestHello_RejectsPipeInIntent: pipe in --intent breaks the parseable body
// format; reject with exit 2.
func TestHello_RejectsPipeInIntent(t *testing.T) {
	base := t.TempDir()
	cmd := lotoCmd(base, flagAgentLong, "Scout",
		subcmdHello, "src/**", flagIntentLong, "foo|bar")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected error, got %s", out)
	}
	if !strings.Contains(string(out), "|") && !strings.Contains(string(out), "pipe") {
		t.Errorf("expected pipe-rejection message, got: %s", out)
	}
}

// TestHello_RejectsBothTiebreakerFlags: --tiebreaker and --no-tiebreaker are
// mutually exclusive.
func TestHello_RejectsBothTiebreakerFlags(t *testing.T) {
	base := t.TempDir()
	cmd := lotoCmd(base, flagAgentLong, "Scout",
		subcmdHello, "src/**", flagIntentLong, "x",
		"--tiebreaker", "y", "--no-tiebreaker")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected error, got %s", out)
	}
}
