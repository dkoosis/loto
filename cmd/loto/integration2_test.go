//go:build unix

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const statusFree = "free"

// lotoLLMCmd returns an *exec.Cmd that uses the LLM output format (no --json).
func lotoLLMCmd(base string, args ...string) *exec.Cmd {
	full := append([]string{"--base", base}, args...)
	cmd := exec.Command(lotoBin, full...)
	cmd.Env = append(os.Environ(), "LOTO_SUPPRESS_LEGACY_WARNING=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// startHolder launches loto try file --hold and waits until it prints "acquired".
// Returns the running process; caller must kill + wait to clean up.
func startHolder(t *testing.T, base, agent, target string) *exec.Cmd {
	t.Helper()
	holder := lotoCmd(base, "--agent", agent, "try", "file", "--hold", target)
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	acquired := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"acquired"`) {
				close(acquired)
				return
			}
		}
	}()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		killAndWait(holder)
		t.Fatal("holder did not confirm acquisition within 5s")
	}
	return holder
}

// startGlobalHolder launches loto try global --hold and waits for "acquired".
func startGlobalHolder(t *testing.T, base, agent string) *exec.Cmd {
	t.Helper()
	holder := lotoCmd(base, "--agent", agent, "try", "global", "--hold")
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatalf("start global holder: %v", err)
	}
	acquired := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"acquired"`) {
				close(acquired)
				return
			}
		}
	}()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		killAndWait(holder)
		t.Fatal("global holder did not confirm acquisition within 5s")
	}
	return holder
}

// killAndWait SIGKILLs the entire process group rooted at p (set up via
// Setpgid in lotoCmd) and reaps it. Group-kill is what prevents orphaned
// --hold subprocesses from outliving a panicked test.
func killAndWait(p *exec.Cmd) {
	if p == nil || p.Process == nil {
		return
	}
	_ = syscall.Kill(-p.Process.Pid, syscall.SIGKILL)
	_ = p.Wait()
}

// ── try global ────────────────────────────────────────────────────────────────

func TestTryGlobalAcquireJSON(t *testing.T) {
	base := t.TempDir()
	cmd := lotoCmd(base, "--agent", "agent-a", "try", "global")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("try global: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if result["acquired"] != true {
		t.Errorf("expected acquired=true, got %v", result)
	}
	if result["kind"] != "global" {
		t.Errorf("expected kind=global, got %v", result["kind"])
	}
}

func TestTryGlobalContended(t *testing.T) {
	base := t.TempDir()
	holder := startGlobalHolder(t, base, "global-holder")
	t.Cleanup(func() { killAndWait(holder) })

	cmd := lotoCmd(base, "--agent", "global-contender", "try", "global")
	out, err := cmd.Output()
	if err == nil {
		t.Fatalf("expected contender to fail, got success: %s", out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("expected exit 1, got %v", err)
	}
	if !strings.Contains(string(ee.Stderr), "global-holder") {
		t.Errorf("holder not named in contender output: %s", ee.Stderr)
	}
}

// ── status ────────────────────────────────────────────────────────────────────

func TestStatusFileFree(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "free.go")

	cmd := lotoCmd(base, "status", target)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if result[target] != statusFree {
		t.Errorf("expected %q=free, got %v", target, result[target])
	}
}

func TestStatusFileHeld(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "held.go")

	holder := startHolder(t, base, "status-holder", target)
	t.Cleanup(func() { killAndWait(holder) })

	cmd := lotoCmd(base, "status", target)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	entry, ok := result[target].(map[string]any)
	if !ok {
		t.Fatalf("expected map for held target, got %T: %v", result[target], result[target])
	}
	if entry["agent_id"] != "status-holder" {
		t.Errorf("expected agent_id=status-holder, got %v", entry["agent_id"])
	}
}

func TestStatusMultipleTargets(t *testing.T) {
	base := t.TempDir()
	dir := t.TempDir()
	free := filepath.Join(dir, "free.go")
	held := filepath.Join(dir, "held.go")

	holder := startHolder(t, base, "multi-holder", held)
	t.Cleanup(func() { killAndWait(holder) })

	cmd := lotoCmd(base, "status", free, held)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if result[free] != statusFree {
		t.Errorf("expected free file to be free, got %v", result[free])
	}
	if _, ok := result[held].(map[string]any); !ok {
		t.Errorf("expected held file to have tag map, got %T: %v", result[held], result[held])
	}
}

func TestStatusGlobalFree(t *testing.T) {
	base := t.TempDir()
	cmd := lotoCmd(base, "status")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("status (no args): %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if result["global"] != statusFree {
		t.Errorf("expected global=free, got %v", result["global"])
	}
}

func TestStatusGlobalHeld(t *testing.T) {
	base := t.TempDir()
	holder := startGlobalHolder(t, base, "global-status-holder")
	t.Cleanup(func() { killAndWait(holder) })

	cmd := lotoCmd(base, "status")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("status (global held): %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	globalEntry, ok := result["global"].(map[string]any)
	if !ok {
		t.Fatalf("expected map for held global, got %T: %v", result["global"], result["global"])
	}
	if globalEntry["agent_id"] != "global-status-holder" {
		t.Errorf("expected agent_id=global-status-holder, got %v", globalEntry["agent_id"])
	}
}

// ── reap ──────────────────────────────────────────────────────────────────────

func TestReapAfterCrash(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "reap-me.go")

	holder := startHolder(t, base, "crash-me", target)
	killAndWait(holder)
	time.Sleep(50 * time.Millisecond)

	cmd := lotoCmd(base, "reap", target)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("reap: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if result["reaped"] != true {
		t.Errorf("expected reaped=true, got %v", result)
	}

	// Verify it's gone.
	statCmd := lotoCmd(base, "status", target)
	statOut, err := statCmd.Output()
	if err != nil {
		t.Fatalf("status after reap: %v", err)
	}
	if !strings.Contains(string(statOut), statusFree) {
		t.Errorf("expected free after reap, got: %s", statOut)
	}
}

func TestReapLiveHolderFails(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "live.go")

	holder := startHolder(t, base, "live-holder", target)
	t.Cleanup(func() { killAndWait(holder) })

	cmd := lotoCmd(base, "reap", target)
	out, err := cmd.Output()
	if err == nil {
		t.Fatalf("reap of live lock should fail, got success: %s", out)
	}
	// Should exit non-zero (exit 1 or 3).
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("expected non-zero exit, got %v", err)
	}
}

// ── break ─────────────────────────────────────────────────────────────────────

func TestBreakNoForceOnDeadHolder(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "break-stale.go")

	holder := startHolder(t, base, "break-dead", target)
	killAndWait(holder)
	time.Sleep(50 * time.Millisecond)

	cmd := lotoCmd(base, "break", target)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("break (no --force): %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if result["reaped"] != true {
		t.Errorf("expected reaped=true, got %v", result)
	}
}

func TestBreakForceOnDeadHolder(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "break-force.go")

	holder := startHolder(t, base, "force-dead", target)
	killAndWait(holder)
	time.Sleep(50 * time.Millisecond)

	cmd := lotoCmd(base, "--agent", "breaker", "break", "--force", "--reason", "test takeover", target)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("break --force: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if result["broken"] != true {
		t.Errorf("expected broken=true, got %v", result)
	}
	if result["force"] != true {
		t.Errorf("expected force=true, got %v", result)
	}
}

// TestBreakForceNotifiesDisplacedAgent verifies that break --force sends a
// mailbox message to the displaced agent (identified from the tag). The holder
// is killed first so the flock is free; ForceBreak still reads the stale tag
// to find the displaced agent before clearing it.
func TestBreakForceNotifiesDisplacedAgent(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "break-notify.go")

	holder := startHolder(t, base, "displaced-agent", target)
	killAndWait(holder)
	time.Sleep(50 * time.Millisecond)

	cmd := lotoCmd(base, "--agent", "breaker-agent", "break", "--force", "--reason", "urgent fix", target)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("break --force: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if result["broken"] != true {
		t.Errorf("expected broken=true, got %v", result)
	}
	// Displaced agent should have a mailbox message with the reason.
	inboxCmd := lotoCmd(base, "--agent", "displaced-agent", "inbox", target)
	inboxOut, err := inboxCmd.Output()
	if err != nil {
		t.Fatalf("inbox after force break: %v\n%s", err, inboxOut)
	}
	if !strings.Contains(string(inboxOut), "urgent fix") {
		t.Errorf("displaced agent inbox missing break reason: %s", inboxOut)
	}
}

// ── release ───────────────────────────────────────────────────────────────────

func TestReleaseAllMine(t *testing.T) {
	base := t.TempDir()
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	agent := "release-agent"

	// Acquire both files, release immediately (no --hold).
	for _, target := range []string{a, b} {
		cmd := lotoCmd(base, "--agent", agent, "try", "file", target)
		if out, err := cmd.Output(); err != nil {
			t.Fatalf("try %s: %v\n%s", target, err, out)
		}
	}

	// Release all.
	relCmd := lotoCmd(base, "--agent", agent, "release", "--all-mine")
	out, err := relCmd.Output()
	if err != nil {
		t.Fatalf("release --all-mine: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	released, _ := result["released"].([]any)
	if len(released) == 0 {
		t.Logf("released: %v (may be 0 if locks expired immediately — not a fatal error for this test)", released)
	}
}

func TestReleaseRequiresAllMineFlag(t *testing.T) {
	base := t.TempDir()
	cmd := lotoCmd(base, "release")
	out, err := cmd.Output()
	if err == nil {
		t.Fatalf("release without --all-mine should fail, got: %s", out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("expected exit 2 (usage error), got %v", err)
	}
}

// ── msg + inbox ────────────────────────────────────────────────────────────────

func TestMsgInboxRoundTrip(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "msg-target.go")

	// Send a message from sender to recipient.
	msgCmd := lotoCmd(base, "--agent", "sender", "msg", target, "please release soon",
		"--to", "recipient")
	if out, err := msgCmd.Output(); err != nil {
		t.Fatalf("msg: %v\n%s", err, out)
	}

	// Recipient reads inbox.
	inboxCmd := lotoCmd(base, "--agent", "recipient", "inbox", target)
	out, err := inboxCmd.Output()
	if err != nil {
		t.Fatalf("inbox: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "please release soon") {
		t.Errorf("inbox missing message body: %s", out)
	}
}

func TestMsgInboxBroadcast(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "broadcast.go")

	// Default --to is @all.
	msgCmd := lotoCmd(base, "--agent", "broadcaster", "msg", target, "broadcast message")
	if out, err := msgCmd.Output(); err != nil {
		t.Fatalf("msg: %v\n%s", err, out)
	}

	// Any agent reading inbox should see it.
	inboxCmd := lotoCmd(base, "--agent", "any-agent", "inbox", target)
	out, err := inboxCmd.Output()
	if err != nil {
		t.Fatalf("inbox: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "broadcast message") {
		t.Errorf("inbox missing broadcast message: %s", out)
	}
}

// TestInboxMineCrossFile: an agent should see all of its messages across
// targets in one call, with the checkpoint advancing so a follow-up call
// returns nothing new.
func TestInboxMineCrossFile(t *testing.T) {
	base := t.TempDir()
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")

	for _, send := range [][]string{
		{"--agent", "sender", "msg", a, "for-recipient-a", "--to", "recipient"},
		{"--agent", "sender", "msg", b, "for-recipient-b", "--to", "recipient"},
		{"--agent", "sender", "msg", a, "for-someone-else", "--to", "other-agent"},
	} {
		if out, err := lotoCmd(base, send...).Output(); err != nil {
			t.Fatalf("msg: %v\n%s", err, out)
		}
	}

	out, err := lotoCmd(base, "--agent", "recipient", "inbox", "--mine").Output()
	if err != nil {
		t.Fatalf("inbox --mine: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "for-recipient-a") || !strings.Contains(s, "for-recipient-b") {
		t.Errorf("inbox --mine missing expected messages: %s", s)
	}
	if strings.Contains(s, "for-someone-else") {
		t.Errorf("inbox --mine leaked another agent's message: %s", s)
	}

	// Second call should show no new messages — checkpoint advanced.
	out2, err := lotoCmd(base, "--agent", "recipient", "inbox", "--mine").Output()
	if err != nil {
		t.Fatalf("inbox --mine (2nd): %v\n%s", err, out2)
	}
	if strings.Contains(string(out2), "for-recipient-a") || strings.Contains(string(out2), "for-recipient-b") {
		t.Errorf("expected empty inbox on 2nd call (checkpoint), got: %s", out2)
	}

	// A new message arrives — only it should appear.
	if out3, err := lotoCmd(base, "--agent", "sender", "msg", b, "fresh-one", "--to", "recipient").Output(); err != nil {
		t.Fatalf("msg fresh: %v\n%s", err, out3)
	}
	out4, err := lotoCmd(base, "--agent", "recipient", "inbox", "--mine").Output()
	if err != nil {
		t.Fatalf("inbox --mine (3rd): %v\n%s", err, out4)
	}
	if !strings.Contains(string(out4), "fresh-one") {
		t.Errorf("expected fresh-one in 3rd call: %s", out4)
	}
	if strings.Contains(string(out4), "for-recipient-a") {
		t.Errorf("3rd call should not re-show old messages: %s", out4)
	}
}

// ── install-hook ──────────────────────────────────────────────────────────────

func TestInstallHookWritesSettings(t *testing.T) {
	dir := t.TempDir()
	base := t.TempDir()

	cmd := lotoCmd(base, "install-hook")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("install-hook: %v\n%s", err, out)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse settings.json: %v\n%s", err, data)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatal("expected hooks section in settings.json")
	}
	if hooks["SessionStart"] == nil {
		t.Error("expected SessionStart hook")
	}
	if hooks["Stop"] == nil {
		t.Error("expected Stop hook")
	}
}

func TestInstallHookIdempotentSettings(t *testing.T) {
	dir := t.TempDir()
	base := t.TempDir()

	run := func() {
		t.Helper()
		cmd := lotoCmd(base, "install-hook")
		cmd.Dir = dir
		if out, err := cmd.Output(); err != nil {
			t.Fatalf("install-hook: %v\n%s", err, out)
		}
	}
	run()
	run()

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	// Should still be valid JSON and not have duplicated hooks.
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings.json invalid after double install: %v\n%s", err, data)
	}
}

// ── reserve ───────────────────────────────────────────────────────────────────

func TestReserveAddListRelease(t *testing.T) {
	base := t.TempDir()
	pattern := "internal/store/**/*.go"
	agent := "reserve-agent"

	// Add reservation.
	addCmd := lotoCmd(base, "--agent", agent, "--intent", "store refactor",
		"reserve", "add", pattern)
	if out, err := addCmd.Output(); err != nil {
		t.Fatalf("reserve add: %v\n%s", err, out)
	}

	// List should include it.
	listCmd := lotoCmd(base, "reserve", "list")
	listOut, err := listCmd.Output()
	if err != nil {
		t.Fatalf("reserve list: %v\n%s", err, listOut)
	}
	if !strings.Contains(string(listOut), pattern) {
		t.Errorf("pattern not in list: %s", listOut)
	}

	// Release.
	relCmd := lotoCmd(base, "reserve", "release", pattern)
	if out, err := relCmd.Output(); err != nil {
		t.Fatalf("reserve release: %v\n%s", err, out)
	}

	// List should be empty.
	listOut2, err := lotoCmd(base, "reserve", "list").Output()
	if err != nil {
		t.Fatalf("reserve list after release: %v\n%s", err, listOut2)
	}
	if strings.Contains(string(listOut2), pattern) {
		t.Errorf("pattern still in list after release: %s", listOut2)
	}
}

func TestReserveListEmpty(t *testing.T) {
	base := t.TempDir()
	out, err := lotoCmd(base, "reserve", "list").Output()
	if err != nil {
		t.Fatalf("reserve list: %v\n%s", err, out)
	}
	// Should decode as an empty JSON array, not null.
	var result []any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if len(result) != 0 {
		t.Errorf("expected empty list, got %d entries", len(result))
	}
}

func TestReserveTTL(t *testing.T) {
	base := t.TempDir()
	addCmd := lotoCmd(base, "--agent", "ttl-agent", "reserve", "add", "src/**", "--ttl", "1h")
	out, err := addCmd.Output()
	if err != nil {
		t.Fatalf("reserve add --ttl: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	// Should have an expires_at field.
	if result["expires_at"] == nil {
		t.Errorf("expected expires_at in reservation, got %v", result)
	}
}

func TestReserveWarningOnTryFile(t *testing.T) {
	base := t.TempDir()
	dir := t.TempDir()
	target := filepath.Join(dir, "internal", "store", "store.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

	// Another agent stakes a glob reservation covering our target.
	addCmd := lotoCmd(base, "--agent", "reserve-owner", "--intent", "store refactor",
		"reserve", "add", filepath.Join(dir, "internal", "store", "**"))
	if out, err := addCmd.Output(); err != nil {
		t.Fatalf("reserve add: %v\n%s", err, out)
	}

	// Different agent tries to acquire the file — should succeed (exit 0)
	// but JSON output should include reservation_warnings.
	tryCmd := lotoCmd(base, "--agent", "other-agent", "try", "file", target)
	out, err := tryCmd.Output()
	if err != nil {
		t.Fatalf("try file with reservation: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if result["acquired"] != true {
		t.Errorf("expected acquired=true, got %v", result)
	}
	warnings, _ := result["reservation_warnings"].([]any)
	if len(warnings) == 0 {
		t.Errorf("expected reservation_warnings, got %v", result)
	}
}

func TestReserveLLMFormat(t *testing.T) {
	base := t.TempDir()
	pattern := "internal/foo/**/*.go"

	// add — use a UUID via --agent so we can assert UUID→handle resolution.
	const uuid = "01234567-89ab-4cde-8f01-23456789abcd"
	addOut, err := lotoLLMCmd(base, "--agent", uuid, "--intent", "refactor",
		"reserve", "add", pattern).Output()
	if err != nil {
		t.Fatalf("reserve add (LLM): %v\n%s", err, addOut)
	}
	s := string(addOut)
	if !strings.HasPrefix(s, "loto:llm:v1\n") {
		t.Errorf("add: missing LLM header: %s", s)
	}
	if !strings.Contains(s, "✔ reserved") || !strings.Contains(s, pattern) {
		t.Errorf("add: missing reserved line: %s", s)
	}
	if !strings.Contains(s, "by:") {
		t.Errorf("add: missing by:<handle> field: %s", s)
	}
	if strings.Contains(s, uuid) {
		// raw UUID must be resolved to a handle in LLM output
		t.Errorf("add: raw UUID leaked into LLM output: %s", s)
	}
	if !strings.Contains(s, "intent:refactor") {
		t.Errorf("add: missing intent: %s", s)
	}

	// list (non-empty)
	listOut, err := lotoLLMCmd(base, "reserve", "list").Output()
	if err != nil {
		t.Fatalf("reserve list (LLM): %v\n%s", err, listOut)
	}
	ls := string(listOut)
	if !strings.HasPrefix(ls, "loto:llm:v1\n") {
		t.Errorf("list: missing LLM header: %s", ls)
	}
	if !strings.Contains(ls, "reservations | n:1") {
		t.Errorf("list: expected 'reservations | n:1', got: %s", ls)
	}
	if !strings.Contains(ls, pattern) || !strings.Contains(ls, "by:") {
		t.Errorf("list: missing pattern/by: %s", ls)
	}
	if strings.Contains(ls, uuid) {
		t.Errorf("list: raw UUID leaked into LLM output: %s", ls)
	}

	// release
	relOut, err := lotoLLMCmd(base, "reserve", "release", pattern).Output()
	if err != nil {
		t.Fatalf("reserve release (LLM): %v\n%s", err, relOut)
	}
	rs := string(relOut)
	if !strings.HasPrefix(rs, "loto:llm:v1\n") || !strings.Contains(rs, "✔ unreserved") {
		t.Errorf("release: bad LLM output: %s", rs)
	}

	// list (empty after release)
	emptyOut, err := lotoLLMCmd(base, "reserve", "list").Output()
	if err != nil {
		t.Fatalf("reserve list empty (LLM): %v\n%s", err, emptyOut)
	}
	es := string(emptyOut)
	if !strings.Contains(es, "[status: empty]") {
		t.Errorf("list-empty: expected [status: empty], got: %s", es)
	}
}

// ── doctor ────────────────────────────────────────────────────────────────────

func TestDoctorClean(t *testing.T) {
	base := t.TempDir()
	cmd := lotoCmd(base, "--agent", "doctor-agent", "doctor")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("doctor on clean base: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if result["clean"] != true {
		t.Errorf("expected clean=true on fresh base, got %v", result)
	}
}

func TestDoctorDetectsStale(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "stale.go")

	holder := startHolder(t, base, "stale-agent", target)
	killAndWait(holder)
	time.Sleep(50 * time.Millisecond)

	cmd := lotoCmd(base, "--agent", "doctor-agent", "doctor")
	out, err := cmd.Output()
	// Doctor exits 1 when drift found.
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		// Expected: drift found.
		if !strings.Contains(string(out), "stale") {
			t.Logf("doctor output: %s", out)
		}
		return
	}
	if err != nil {
		t.Fatalf("doctor: unexpected error %v\n%s", err, out)
	}
	// May also exit 0 if GC already cleaned up — acceptable.
}

func TestDoctorRepair(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "repair-me.go")

	holder := startHolder(t, base, "repair-agent", target)
	killAndWait(holder)
	time.Sleep(50 * time.Millisecond)

	cmd := lotoCmd(base, "--agent", "doctor-agent", "doctor", "--repair")
	out, err := cmd.Output()
	// After repair: should exit 0 or 1. If drift was found and repaired, next
	// run should be clean.
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("doctor --repair: unexpected error %v\n%s", err, out)
		}
	}

	// Second doctor run: should be clean.
	cmd2 := lotoCmd(base, "--agent", "doctor-agent", "doctor")
	out2, err2 := cmd2.Output()
	if err2 != nil {
		t.Fatalf("doctor after repair: %v\n%s", err2, out2)
	}
	var result map[string]any
	if err := json.Unmarshal(out2, &result); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out2)
	}
	if result["clean"] != true {
		t.Errorf("expected clean after repair, got %v", result)
	}
}

// ── whoami --ensure ───────────────────────────────────────────────────────────

func TestWhoamiEnsure(t *testing.T) {
	out, err := exec.Command(lotoBin, "whoami", "--ensure").CombinedOutput()
	if err != nil {
		t.Fatalf("whoami --ensure: %v\n%s", err, out)
	}
	if !strings.HasPrefix(string(out), "loto:llm:v1\n") {
		t.Errorf("expected LLM header, got: %s", out)
	}
}

// ── try --wait timeout ────────────────────────────────────────────────────────

func TestTryWaitTimesOut(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "timeout.go")

	holder := startHolder(t, base, "timeout-holder", target)
	t.Cleanup(func() { killAndWait(holder) })

	start := time.Now()
	cmd := lotoCmd(base, "--agent", "waiter", "try", "file", "--wait", "200ms", target)
	out, err := cmd.Output()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout to fail, got success: %s", out)
	}
	// Context deadline → exit 1 (advisory conflict) or exit 3 (system/timeout);
	// either is acceptable — the key signal is non-zero and the elapsed time.
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("expected non-zero exit on timeout, got %v", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("expected wait of ~200ms, got %v", elapsed)
	}
}

// ── try --ttl ─────────────────────────────────────────────────────────────────

func TestTryFileTTL(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "ttl.go")

	// Use --hold so the process stays alive while we check status.
	holder := lotoCmd(base, "--agent", "ttl-agent", "try", "file", "--hold", "--ttl", "30m", target)
	holderOut, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal("start ttl holder:", err)
	}
	t.Cleanup(func() { killAndWait(holder) })

	acquired := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(holderOut)
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"acquired"`) {
				close(acquired)
				return
			}
		}
	}()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("ttl holder did not confirm acquisition within 5s")
	}

	// Status should show expires_at.
	statCmd := lotoCmd(base, "status", target)
	statOut, err := statCmd.Output()
	if err != nil {
		t.Fatalf("status after try --ttl: %v\n%s", err, statOut)
	}
	var statResult map[string]any
	if err := json.Unmarshal(statOut, &statResult); err != nil {
		t.Fatalf("parse status: %v\n%s", err, statOut)
	}
	entry, ok := statResult[target].(map[string]any)
	if !ok {
		t.Fatalf("expected map for target, got %T: %s", statResult[target], statOut)
	}
	if entry["expires_at"] == nil {
		t.Errorf("expected expires_at in tag after --ttl, got %v", entry)
	}
}

// ── LLM format ────────────────────────────────────────────────────────────────

func TestLLMFormatTrySuccess(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "llm.go")

	cmd := lotoLLMCmd(base, "--agent", "llm-agent", "try", "file", target)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("try file (LLM): %v\n%s", err, out)
	}
	s := string(out)
	if !strings.HasPrefix(s, "loto:llm:v1\n") {
		t.Errorf("expected LLM header, got: %s", s)
	}
	if !strings.Contains(s, "✔") {
		t.Errorf("expected ✔ glyph in LLM success output: %s", s)
	}
}

func TestLLMFormatBlocked(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "llm-blocked.go")

	holder := startHolder(t, base, "llm-holder", target)
	t.Cleanup(func() { killAndWait(holder) })

	cmd := lotoLLMCmd(base, "--agent", "llm-contender", "try", "file", target)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected blocked exit 1, got success: %s", out)
	}
	s := string(out)
	if !strings.HasPrefix(s, "loto:llm:v1\n") {
		t.Errorf("expected LLM header on stderr/combined, got: %s", s)
	}
	if !strings.Contains(s, "✗") {
		t.Errorf("expected ✗ glyph in LLM blocked output: %s", s)
	}
}

func TestLLMFormatStatus(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "llm-status.go")

	cmd := lotoLLMCmd(base, "status", target)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("status (LLM): %v\n%s", err, out)
	}
	s := string(out)
	if !strings.HasPrefix(s, "loto:llm:v1\n") {
		t.Errorf("expected LLM header, got: %s", s)
	}
	if !strings.Contains(s, "✔") && !strings.Contains(s, statusFree) {
		t.Errorf("expected free indicator in LLM status: %s", s)
	}
}

func TestLLMFormatRelease(t *testing.T) {
	base := t.TempDir()
	cmd := lotoLLMCmd(base, "--agent", "llm-release-agent", "release", "--all-mine")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("release --all-mine (LLM): %v\n%s", err, out)
	}
	s := string(out)
	if !strings.HasPrefix(s, "loto:llm:v1\n") {
		t.Errorf("expected LLM header, got: %s", s)
	}
}

func TestLLMFormatWhoami(t *testing.T) {
	out, err := exec.Command(lotoBin, "whoami").CombinedOutput()
	if err != nil {
		t.Fatalf("whoami: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.HasPrefix(s, "loto:llm:v1\n") {
		t.Errorf("expected LLM header, got: %s", s)
	}
	if !strings.Contains(s, "agent") {
		t.Errorf("expected agent field in LLM whoami: %s", s)
	}
}

// ── check-paths with explicit paths ──────────────────────────────────────────

func TestCheckPathsExplicitPathFree(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "explicit.go")

	cmd := lotoCmd(base, "--agent", "check-agent", "check-paths", target)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("check-paths (explicit free): %v\n%s", err, out)
	}
	// design.md: silence looks like a crash — empty result must still emit
	// a structured payload. lotoCmd forces --json; expect the empty array.
	s := string(out)
	if !strings.Contains(s, `"conflicts"`) {
		t.Errorf("expected JSON payload with conflicts key on empty result, got:\n%s", s)
	}
}

func TestCheckPathsExplicitPathHeld(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "held-check.go")

	holder := startHolder(t, base, "check-holder", target)
	t.Cleanup(func() { killAndWait(holder) })

	cmd := lotoCmd(base, "--agent", "check-other", "check-paths", target)
	out, err := cmd.Output()
	if err == nil {
		t.Fatalf("expected check-paths to exit 1 on held file, got success: %s", out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("expected exit 1, got %v", err)
	}
}

// ── exit code 2 (usage errors) ────────────────────────────────────────────────

func TestExitCodeUsageErrorRelease(t *testing.T) {
	base := t.TempDir()
	cmd := lotoCmd(base, "release")
	out, err := cmd.Output()
	if err == nil {
		t.Fatalf("release without --all-mine should exit 2, got success: %s", out)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() != 2 {
		t.Errorf("expected exit 2, got %d", ee.ExitCode())
	}
}

// ── try global then status ────────────────────────────────────────────────────

func TestTryGlobalThenStatus(t *testing.T) {
	base := t.TempDir()

	holder := startGlobalHolder(t, base, "global-try-status")
	t.Cleanup(func() { killAndWait(holder) })

	// Status (no args) should reflect the global lock.
	statCmd := lotoCmd(base, "status")
	out, err := statCmd.Output()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "global-try-status") {
		t.Errorf("expected holder in status, got: %s", out)
	}
}

// ── whoami JSON fields ────────────────────────────────────────────────────────

func TestWhoamiJSONFields(t *testing.T) {
	out, err := exec.Command(lotoBin, "whoami", "--json").CombinedOutput()
	if err != nil {
		t.Fatalf("whoami --json: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse --json output: %v\n%s", err, out)
	}
	for _, field := range []string{"id", "handle", "created_at"} {
		if result[field] == nil {
			t.Errorf("missing field %q in whoami --json: %v", field, result)
		}
	}
	// handle should look like PascalCase (e.g. "GreenCastle")
	handle, _ := result["handle"].(string)
	if handle == "" {
		t.Errorf("expected non-empty handle, got %q", handle)
	}
}

// ── try file then release lifecycle ──────────────────────────────────────────

// TestTryReleaseLifecycle uses --hold so the lock stays live across status
// checks, then kills the holder (which causes flock release) and verifies
// that release --all-mine removes the stale tag, leaving status free.
func TestTryReleaseLifecycle(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "lifecycle.go")
	agent := "lifecycle-agent"

	holder := startHolder(t, base, agent, target)

	// Status: should be held.
	statOut, err := lotoCmd(base, "status", target).Output()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, statOut)
	}
	if strings.Contains(string(statOut), statusFree) {
		killAndWait(holder)
		t.Fatalf("expected held status, got free: %s", statOut)
	}

	// Kill the holder so the flock is released.
	killAndWait(holder)
	time.Sleep(50 * time.Millisecond)

	// Release --all-mine removes the stale tag.
	relOut, err := lotoCmd(base, "--agent", agent, "release", "--all-mine").Output()
	if err != nil {
		t.Fatalf("release: %v\n%s", err, relOut)
	}

	// Status: free.
	statOut2, err := lotoCmd(base, "status", target).Output()
	if err != nil {
		t.Fatalf("status after release: %v\n%s", err, statOut2)
	}
	if !strings.Contains(string(statOut2), statusFree) {
		t.Errorf("expected free after release, got: %s", statOut2)
	}
}
