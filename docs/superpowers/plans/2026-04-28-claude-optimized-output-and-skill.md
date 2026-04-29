# Claude-Optimized Output, Global Hooks, and loto Skill — Implementation Plan

> **For agentic workers:** Use dk:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make loto consumable by Claude across every project: add a token-dense `--llm` output format conforming to the Claude-Optimized Utility Output standard (nug 32f0ece29b72), promote loto's hooks to `~/.claude/settings.json` so every session gets identity + auto-release, then ship a `~/.claude/skills/loto.md` skill that teaches the operating loop using `--llm`.

**Architecture:** Single chokepoint refactor. Today every command calls `printJSON(v)` in `cmd/loto/main.go`. Replace with per-shape `emit*` helpers that dispatch on a global `currentFormat` to either JSON or LLM. Default format becomes `llm` when stdout is non-tty (today: `json`), preserving `--json` as an explicit override for back-compat (the existing project hooks rely on it). Tty default stays JSON for now — this plan does not change human-facing output. The `ErrHeld` blocker path on stderr gets the same treatment.

‡ **Engineer note — Go single-import rule.** The code snippets in Tasks 2, 3, and 5 each show an `import (...)` block when adding code to `internal/render/llm.go`. Go allows only one import block per file. Treat the second-and-later import lists as **additions to merge into the existing import block**, not new blocks. Same applies to `internal/render/llm_test.go` and to `cmd/loto/main.go`.

‡ **Engineer note — back-compat for `release` JSON shape.** `loto release --all-mine --json` today emits `{"agent":..., "released":["path1","path2"], "errors":[...]}` — the released field is a slice of paths, not a count. The LLM wrapper must keep that slice intact in the JSON branch and derive the count only for the LLM line.

**Tech Stack:** Go 1.22+, cobra, existing loto types (`Tag`, `ErrHeld`). Reference standard: nug `32f0ece29b72` Claude-Optimized Utility Output. Symbol glossary: nug `c75320ff5718`.

---

## File Structure

**New:**
- `internal/render/render.go` — `Format` type, `Renderer` interface, format selection
- `internal/render/llm.go` — LLM-format rendering for each output shape
- `internal/render/llm_test.go` — golden tests per shape
- `~/.claude/skills/loto.md` — the skill (filesystem outside repo)

**Modified:**
- `cmd/loto/main.go` — replace `printJSON(...)` calls with `render.Emit(...)`, wire `--format` flag, switch no-tty default
- `cmd/loto/main.go` — `exit()` for `ErrHeld` uses renderer for stderr blocker output
- `loto.go` — add a small accessor on `ErrHeld` if needed for non-JSON rendering (only if direct field access is awkward)
- `README.md` — mention `--llm` as default for non-tty
- `~/.claude/settings.json` — add SessionStart + Stop hooks (lifted from project)

**Untouched but verified:**
- existing project `.claude/settings.json` — keeps `--json` flag explicit; hooks survive the default flip

---

## Phase 1 — `--llm` Output Format

### Task 1: Scaffold the render package

**Files:**
- Create: `/Users/vcto/Projects/loto/internal/render/render.go`
- Create: `/Users/vcto/Projects/loto/internal/render/render_test.go`

- [ ] **Step 1: Create the package skeleton with format enum and selector**

```go
// /Users/vcto/Projects/loto/internal/render/render.go
// Package render emits loto command output in either JSON (machine/legacy)
// or LLM (token-dense, Claude-optimized) form. See nug 32f0ece29b72.
package render

import (
	"encoding/json"
	"io"
	"os"
)

type Format int

const (
	FormatJSON Format = iota
	FormatLLM
)

// Resolve picks the output format based on explicit user choice and tty state.
// explicit: "json" | "llm" | "" (auto). When auto, non-tty → LLM, tty → JSON.
func Resolve(explicit string, stdout *os.File) Format {
	switch explicit {
	case "json":
		return FormatJSON
	case "llm":
		return FormatLLM
	}
	if fi, err := stdout.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		return FormatLLM
	}
	return FormatJSON
}

// EmitJSON writes v as indented JSON to w.
func EmitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
```

- [ ] **Step 2: Write the failing test for Resolve**

```go
// /Users/vcto/Projects/loto/internal/render/render_test.go
package render

import (
	"os"
	"testing"
)

func TestResolveExplicitJSON(t *testing.T) {
	if got := Resolve("json", os.Stdout); got != FormatJSON {
		t.Fatalf("explicit json: got %v want FormatJSON", got)
	}
}

func TestResolveExplicitLLM(t *testing.T) {
	if got := Resolve("llm", os.Stdout); got != FormatLLM {
		t.Fatalf("explicit llm: got %v want FormatLLM", got)
	}
}

func TestResolveAutoNonTTY(t *testing.T) {
	// Pipe is non-tty.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if got := Resolve("", w); got != FormatLLM {
		t.Fatalf("auto non-tty: got %v want FormatLLM", got)
	}
}
```

- [ ] **Step 3: Run tests and verify pass**

```bash
cd /Users/vcto/Projects/loto && go test ./internal/render/...
```

Expected: PASS, 3 tests.

- [ ] **Step 4: Commit**

```bash
cd /Users/vcto/Projects/loto && git add internal/render/render.go internal/render/render_test.go
git commit -m "feat(render): scaffold format selector + json emitter"
```

---

### Task 2: LLM renderer for `whoami`

**Files:**
- Create: `/Users/vcto/Projects/loto/internal/render/llm.go`
- Modify: `/Users/vcto/Projects/loto/internal/render/llm_test.go` (new file)

The LLM whoami line is one row: `agent | <handle> | id:<short> | host:<host>`. Format header per standard: `loto:llm:v1`.

- [ ] **Step 1: Write failing test**

```go
// /Users/vcto/Projects/loto/internal/render/llm_test.go
package render

import (
	"bytes"
	"strings"
	"testing"
)

type whoamiInput struct {
	ID     string
	Handle string
	Host   string
}

func TestEmitLLMWhoami(t *testing.T) {
	var buf bytes.Buffer
	in := whoamiInput{ID: "2dd46381-9c26-4c01-97ce-91beda0103d1", Handle: "RemoteSnipe", Host: "Mac"}
	if err := EmitLLMWhoami(&buf, in.ID, in.Handle, in.Host); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	wantHeader := "loto:llm:v1\n"
	if !strings.HasPrefix(got, wantHeader) {
		t.Fatalf("missing header; got:\n%s", got)
	}
	if !strings.Contains(got, "agent | RemoteSnipe | id:2dd46381 | host:Mac\n") {
		t.Fatalf("unexpected body:\n%s", got)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

```bash
cd /Users/vcto/Projects/loto && go test ./internal/render/... -run TestEmitLLMWhoami
```

Expected: FAIL with "EmitLLMWhoami undefined".

- [ ] **Step 3: Implement `EmitLLMWhoami`**

```go
// /Users/vcto/Projects/loto/internal/render/llm.go
package render

import (
	"fmt"
	"io"
)

const llmHeader = "loto:llm:v1\n"

// shortID returns the first 8 chars of a UUID-ish string for display.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// EmitLLMWhoami writes the whoami output in LLM format.
// Layout: "agent | <handle> | id:<short> | host:<host>"
func EmitLLMWhoami(w io.Writer, id, handle, host string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "agent | %s | id:%s | host:%s\n", handle, shortID(id), host)
	return err
}
```

- [ ] **Step 4: Run test, verify pass**

```bash
cd /Users/vcto/Projects/loto && go test ./internal/render/... -run TestEmitLLMWhoami
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/vcto/Projects/loto && git add internal/render/llm.go internal/render/llm_test.go
git commit -m "feat(render): llm format for whoami"
```

---

### Task 3: LLM renderer for `try` success + holder-blocked

The two highest-leverage shapes. Success keeps it short. Holder-blocked is the one Claude reads under stress — token density matters most here.

**Files:**
- Modify: `/Users/vcto/Projects/loto/internal/render/llm.go`
- Modify: `/Users/vcto/Projects/loto/internal/render/llm_test.go`

Layouts:
- **try success:**
  ```
  loto:llm:v1
  ✔ acquired | file | <relpath-or-target> | by:<agent>
  ```
  When `reservation_warnings` present, append one line per warning:
  ```
  ⚠ reservation | <pattern> | held-by:<agent>
  ```
- **holder-blocked (stderr):**
  ```
  loto:llm:v1
  ✗ blocked | <kind> | <target> | by:<agent> | intent:<intent> | held-since:<RFC3339> [| ttl:<expires_at>] [| branch:<b>] [| host:<h>] [| pid:<n>]
  ```
  Truncate `intent` at 80 chars with `…`.

- [ ] **Step 1: Write failing tests**

‡ Merge `"time"` into the existing `import` block in `llm_test.go` from Task 2 — do NOT add a second `import` statement.

```go
// append to /Users/vcto/Projects/loto/internal/render/llm_test.go
// (existing imports: "bytes", "strings", "testing"; add: "time")

func TestEmitLLMTrySuccess(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMTrySuccess(&buf, "file", "internal/store/store.go", "GreenCastle", nil); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "✔ acquired | file | internal/store/store.go | by:GreenCastle\n") {
		t.Fatalf("unexpected:\n%s", got)
	}
}

func TestEmitLLMTrySuccessWithReservationWarnings(t *testing.T) {
	var buf bytes.Buffer
	warnings := []ReservationWarning{
		{Pattern: "internal/store/**", AgentID: "BlueOak"},
	}
	if err := EmitLLMTrySuccess(&buf, "file", "internal/store/store.go", "GreenCastle", warnings); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "⚠ reservation | internal/store/** | held-by:BlueOak\n") {
		t.Fatalf("missing warning line:\n%s", got)
	}
}

func TestEmitLLMBlocked(t *testing.T) {
	var buf bytes.Buffer
	heldSince := time.Date(2026, 4, 28, 14, 32, 11, 0, time.UTC)
	expires := time.Date(2026, 4, 28, 14, 42, 11, 0, time.UTC)
	in := BlockedInput{
		Kind: "file", Target: "internal/store/store.go",
		AgentID: "GreenCastle", Intent: "store refactor",
		HeldSince: heldSince, ExpiresAt: expires,
		Branch: "store-refactor", Host: "dk-mac", PID: 84231,
	}
	if err := EmitLLMBlocked(&buf, in); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	want := "✗ blocked | file | internal/store/store.go | by:GreenCastle | intent:store refactor | held-since:2026-04-28T14:32:11Z | ttl:2026-04-28T14:42:11Z | branch:store-refactor | host:dk-mac | pid:84231\n"
	if !strings.Contains(got, want) {
		t.Fatalf("blocked line mismatch.\nwant: %q\ngot:  %q", want, got)
	}
}

func TestEmitLLMBlockedTruncatesLongIntent(t *testing.T) {
	var buf bytes.Buffer
	long := strings.Repeat("x", 200)
	in := BlockedInput{Kind: "file", Target: "f.go", AgentID: "A", Intent: long, HeldSince: time.Unix(0, 0).UTC()}
	if err := EmitLLMBlocked(&buf, in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "intent:"+strings.Repeat("x", 79)+"…") {
		t.Fatalf("intent not truncated; got:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

```bash
cd /Users/vcto/Projects/loto && go test ./internal/render/...
```

Expected: FAIL — `EmitLLMTrySuccess`, `EmitLLMBlocked`, `ReservationWarning`, `BlockedInput` undefined.

- [ ] **Step 3: Implement**

‡ Merge `"strings"` and `"time"` into the existing `import` block in `llm.go` from Task 2 — do NOT add a second `import` statement.

```go
// append to /Users/vcto/Projects/loto/internal/render/llm.go
// (existing imports: "fmt", "io"; add: "strings", "time")

// ReservationWarning describes a soft conflict surfaced after a successful acquire.
type ReservationWarning struct {
	Pattern string
	AgentID string
}

// BlockedInput is the data needed to render a holder-blocked report.
type BlockedInput struct {
	Kind      string // "file" | "global"
	Target    string
	AgentID   string
	Intent    string
	HeldSince time.Time
	ExpiresAt time.Time // zero = no TTL
	Branch    string
	Host      string
	PID       int
}

const intentMax = 80

func truncIntent(s string) string {
	if len(s) <= intentMax {
		return s
	}
	return s[:intentMax-1] + "…"
}

// EmitLLMTrySuccess writes a one-line acquired record plus optional reservation warnings.
func EmitLLMTrySuccess(w io.Writer, kind, target, agent string, warnings []ReservationWarning) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "✔ acquired | %s | %s | by:%s\n", kind, target, agent); err != nil {
		return err
	}
	for _, rw := range warnings {
		if _, err := fmt.Fprintf(w, "⚠ reservation | %s | held-by:%s\n", rw.Pattern, rw.AgentID); err != nil {
			return err
		}
	}
	return nil
}

// EmitLLMBlocked writes a holder-blocked report. Optional fields are appended
// only when non-zero.
func EmitLLMBlocked(w io.Writer, in BlockedInput) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✗ blocked | %s | %s | by:%s | intent:%s | held-since:%s",
		in.Kind, in.Target, in.AgentID, truncIntent(in.Intent),
		in.HeldSince.UTC().Format(time.RFC3339))
	if !in.ExpiresAt.IsZero() {
		fmt.Fprintf(&b, " | ttl:%s", in.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if in.Branch != "" {
		fmt.Fprintf(&b, " | branch:%s", in.Branch)
	}
	if in.Host != "" {
		fmt.Fprintf(&b, " | host:%s", in.Host)
	}
	if in.PID != 0 {
		fmt.Fprintf(&b, " | pid:%d", in.PID)
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}
```

- [ ] **Step 4: Run tests, verify pass**

```bash
cd /Users/vcto/Projects/loto && go test ./internal/render/...
```

Expected: PASS, all four new tests + earlier tests.

- [ ] **Step 5: Commit**

```bash
cd /Users/vcto/Projects/loto && git add internal/render/llm.go internal/render/llm_test.go
git commit -m "feat(render): llm format for try success + holder-blocked"
```

---

### Task 4: LLM renderer for `status`

**Files:**
- Modify: `/Users/vcto/Projects/loto/internal/render/llm.go`
- Modify: `/Users/vcto/Projects/loto/internal/render/llm_test.go`

Layouts:
- **No args (global only), free:**
  ```
  loto:llm:v1
  ✔ global | free
  ```
- **No args, held:**
  ```
  loto:llm:v1
  ✗ global | by:<agent> | intent:<intent>
  ```
- **With targets, table form (positional, no per-row keys):**
  ```
  loto:llm:v1
  status | target | holder | intent
  ✔ free   | <path> | -        | -
  ✗ held   | <path> | <agent>  | <intent>
  ```

- [ ] **Step 1: Write failing tests**

```go
// append to llm_test.go
type statusEntry struct {
	Target  string
	Free    bool
	AgentID string
	Intent  string
}

func TestEmitLLMStatusGlobalFree(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMStatusGlobal(&buf, true, "", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ global | free\n") {
		t.Fatalf("unexpected:\n%s", buf.String())
	}
}

func TestEmitLLMStatusGlobalHeld(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMStatusGlobal(&buf, false, "GreenCastle", "sweep"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✗ global | by:GreenCastle | intent:sweep\n") {
		t.Fatalf("unexpected:\n%s", buf.String())
	}
}

func TestEmitLLMStatusTargets(t *testing.T) {
	var buf bytes.Buffer
	entries := []StatusEntry{
		{Target: "a.go", Free: true},
		{Target: "b.go", Free: false, AgentID: "GreenCastle", Intent: "store refactor"},
	}
	if err := EmitLLMStatusTargets(&buf, entries); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "status | target | holder | intent\n") {
		t.Fatalf("missing column header:\n%s", got)
	}
	if !strings.Contains(got, "✔ free | a.go | - | -\n") {
		t.Fatalf("missing free row:\n%s", got)
	}
	if !strings.Contains(got, "✗ held | b.go | GreenCastle | store refactor\n") {
		t.Fatalf("missing held row:\n%s", got)
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

```bash
cd /Users/vcto/Projects/loto && go test ./internal/render/...
```

Expected: FAIL on undefined symbols.

- [ ] **Step 3: Implement**

```go
// append to llm.go

// StatusEntry is one row of `loto status <targets...>` output.
type StatusEntry struct {
	Target  string
	Free    bool
	AgentID string
	Intent  string
}

// EmitLLMStatusGlobal writes the global-lock status line.
func EmitLLMStatusGlobal(w io.Writer, free bool, agent, intent string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	if free {
		_, err := io.WriteString(w, "✔ global | free\n")
		return err
	}
	_, err := fmt.Fprintf(w, "✗ global | by:%s | intent:%s\n", agent, intent)
	return err
}

// EmitLLMStatusTargets writes a small positional table for per-file status.
func EmitLLMStatusTargets(w io.Writer, entries []StatusEntry) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "status | target | holder | intent\n"); err != nil {
		return err
	}
	for _, e := range entries {
		if e.Free {
			if _, err := fmt.Fprintf(w, "✔ free | %s | - | -\n", e.Target); err != nil {
				return err
			}
			continue
		}
		intent := e.Intent
		if intent == "" {
			intent = "-"
		}
		if _, err := fmt.Fprintf(w, "✗ held | %s | %s | %s\n", e.Target, e.AgentID, intent); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run, verify PASS**

```bash
cd /Users/vcto/Projects/loto && go test ./internal/render/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/vcto/Projects/loto && git add internal/render/llm.go internal/render/llm_test.go
git commit -m "feat(render): llm format for status (global + per-target)"
```

---

### Task 5: LLM renderer for `inbox`, `msg`, `release`, `reap`/`break`, `install-hook`

These are smaller shapes. Bundle them into one task with one test per shape. No-message inbox is the empty-section case from the standard: header + explicit `[status: empty]` line, never silent.

**Files:**
- Modify: `/Users/vcto/Projects/loto/internal/render/llm.go`
- Modify: `/Users/vcto/Projects/loto/internal/render/llm_test.go`

Layouts:
- **inbox empty:** `inbox | <target> | [status: empty]`
- **inbox with msgs:** `inbox | <target> | <N> msgs` then per-msg: `→ from:<agent> | to:<recipient> | <body>` (one line, body collapsed to first 200 chars + `…` if longer)
- **msg sent:** `✔ msg-sent | target:<t> | to:<recipient>`
- **release:** `✔ released | agent:<a> | n:<count>` plus per-error `✗ release-error | <err>`
- **reap:** `✔ reaped | <target>`
- **break --force:** `✔ broken | <target> | by:<a> | reason:<r>`
- **install-hook:** `✔ installed | .claude/settings.json`

- [ ] **Step 1: Write tests for all six shapes**

```go
// append to llm_test.go

func TestEmitLLMInboxEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMInbox(&buf, "store.go", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "inbox | store.go | [status: empty]\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}

func TestEmitLLMInboxWithMessages(t *testing.T) {
	var buf bytes.Buffer
	msgs := []InboxMessage{
		{From: "BlueOak", To: "@all", Body: "renaming Foo→Bar"},
	}
	if err := EmitLLMInbox(&buf, "store.go", msgs); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "inbox | store.go | 1 msgs\n") {
		t.Fatalf("missing header row:\n%s", got)
	}
	if !strings.Contains(got, "→ from:BlueOak | to:@all | renaming Foo→Bar\n") {
		t.Fatalf("missing msg row:\n%s", got)
	}
}

func TestEmitLLMMsgSent(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMMsgSent(&buf, "store.go", "BlueOak"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ msg-sent | target:store.go | to:BlueOak\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}

func TestEmitLLMReleased(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMReleased(&buf, "GreenCastle", 3, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ released | agent:GreenCastle | n:3\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}

func TestEmitLLMReleasedWithErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMReleased(&buf, "A", 1, []string{"permission denied"}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "✗ release-error | permission denied\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestEmitLLMReaped(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMReaped(&buf, "store.go"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ reaped | store.go\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}

func TestEmitLLMBroken(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMBroken(&buf, "store.go", "RedRiver", "stuck"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ broken | store.go | by:RedRiver | reason:stuck\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}

func TestEmitLLMInstalled(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMInstalled(&buf, ".claude/settings.json"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ installed | .claude/settings.json\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

```bash
cd /Users/vcto/Projects/loto && go test ./internal/render/...
```

Expected: FAIL on the new symbols.

- [ ] **Step 3: Implement all six**

```go
// append to llm.go

// InboxMessage is a single message rendered for `loto inbox`.
type InboxMessage struct {
	From string
	To   string
	Body string
}

const inboxBodyMax = 200

func collapseBody(s string) string {
	// Replace newlines so each msg renders on one line.
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= inboxBodyMax {
		return s
	}
	return s[:inboxBodyMax-1] + "…"
}

func EmitLLMInbox(w io.Writer, target string, msgs []InboxMessage) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	if len(msgs) == 0 {
		_, err := fmt.Fprintf(w, "inbox | %s | [status: empty]\n", target)
		return err
	}
	if _, err := fmt.Fprintf(w, "inbox | %s | %d msgs\n", target, len(msgs)); err != nil {
		return err
	}
	for _, m := range msgs {
		if _, err := fmt.Fprintf(w, "→ from:%s | to:%s | %s\n", m.From, m.To, collapseBody(m.Body)); err != nil {
			return err
		}
	}
	return nil
}

func EmitLLMMsgSent(w io.Writer, target, to string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ msg-sent | target:%s | to:%s\n", target, to)
	return err
}

func EmitLLMReleased(w io.Writer, agent string, n int, errs []string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "✔ released | agent:%s | n:%d\n", agent, n); err != nil {
		return err
	}
	for _, e := range errs {
		if _, err := fmt.Fprintf(w, "✗ release-error | %s\n", e); err != nil {
			return err
		}
	}
	return nil
}

func EmitLLMReaped(w io.Writer, target string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ reaped | %s\n", target)
	return err
}

func EmitLLMBroken(w io.Writer, target, by, reason string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ broken | %s | by:%s | reason:%s\n", target, by, reason)
	return err
}

func EmitLLMInstalled(w io.Writer, path string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ installed | %s\n", path)
	return err
}
```

- [ ] **Step 4: Run, verify PASS**

```bash
cd /Users/vcto/Projects/loto && go test ./internal/render/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/vcto/Projects/loto && git add internal/render/llm.go internal/render/llm_test.go
git commit -m "feat(render): llm format for inbox, msg, release, reap, break, install-hook"
```

---

### Task 6: Wire the renderer into `cmd/loto/main.go`

Replace `printJSON(...)` per-call with format dispatch. Add `--format` flag (auto|json|llm). Keep `--json` for back-compat as a synonym for `--format=json`. Switch the auto-when-no-tty default from JSON to LLM.

**Files:**
- Modify: `/Users/vcto/Projects/loto/cmd/loto/main.go`

- [ ] **Step 1: Replace flags + add format resolution**

In `cmd/loto/main.go`, replace lines 17-23 globals + the `init()` flag registration + `PersistentPreRunE`:

```go
// /Users/vcto/Projects/loto/cmd/loto/main.go (replace var block + init flag setup + PersistentPreRunE)

import (
	// ... existing imports ...
	"loto/internal/render"
)

var (
	flagBase   string
	flagAgent  string
	flagIntent string
	flagJSON   bool   // back-compat: synonym for --format=json
	flagFormat string // "" (auto) | "json" | "llm"
	currentFormat render.Format
)

// init flag block:
pf.StringVar(&flagBase, "base", defaultBase(), "coordination base directory (or $LOTO_BASE)")
pf.StringVar(&flagAgent, "agent", defaultAgent(), "agent id")
pf.StringVar(&flagIntent, "intent", "ad-hoc", "human-readable intent")
pf.BoolVar(&flagJSON, "json", false, "force JSON output (alias for --format=json)")
pf.StringVar(&flagFormat, "format", "", "output format: json | llm (default: llm when stdout is not a tty)")

// PersistentPreRunE:
PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
	explicit := flagFormat
	if flagJSON && explicit == "" {
		explicit = "json"
	}
	currentFormat = render.Resolve(explicit, os.Stdout)
	return nil
},
```

- [ ] **Step 2: Add a `render` helper alongside `printJSON`**

In `cmd/loto/main.go`, replace `printJSON` and add a per-shape dispatcher. For brevity, keep the JSON path as before; add an LLM branch per command. Add this helper near the bottom of the file:

```go
// emitWhoami picks the format and writes whoami output to stdout.
// Note: Agent is defined in package main (cmd/loto/identity.go), so it's the
// local type, not loto.Agent.
func emitWhoami(a *Agent) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMWhoami(os.Stdout, a.ID, a.Handle, a.Host)
		return
	}
	_ = render.EmitJSON(os.Stdout, a)
}

func emitTrySuccess(kind, target, agent string, warnings []loto.Reservation) {
	if currentFormat == render.FormatLLM {
		w := make([]render.ReservationWarning, len(warnings))
		for i, r := range warnings {
			w[i] = render.ReservationWarning{Pattern: r.Pattern, AgentID: r.AgentID}
		}
		_ = render.EmitLLMTrySuccess(os.Stdout, kind, target, agent, w)
		return
	}
	result := map[string]any{"acquired": true, "target": target, "agent": agent}
	if kind == "global" {
		result = map[string]any{"acquired": true, "kind": "global", "agent": agent}
	}
	if len(warnings) > 0 {
		patterns := make([]string, len(warnings))
		for i, r := range warnings {
			patterns[i] = r.Pattern + " (" + r.AgentID + ")"
		}
		result["reservation_warnings"] = patterns
	}
	_ = render.EmitJSON(os.Stdout, result)
}
```

- [ ] **Step 3: Repoint each command at its emit helper**

Replace the `printJSON(...)` call sites:

```go
// tryFileCmd RunE — replace lines around 118-133
emitTrySuccess("file", target, flagAgent, lock.Conflicts)
if tryFileHold {
	waitForSignal()
}
_ = lock.Unlock()
return nil

// tryGlobalCmd RunE — replace lines around 148-156
emitTrySuccess("global", "global", flagAgent, nil)
if tryFileHold {
	waitForSignal()
}
_ = lock.Unlock()
return nil

// statusCmd RunE — replace lines around 219-241
if len(args) == 0 {
	tag, err := l.ReadGlobalTag()
	if err != nil {
		emitStatusGlobal(true, "", "")
		return nil
	}
	emitStatusGlobal(false, tag.AgentID, tag.Intent)
	return nil
}
entries := make([]render.StatusEntry, 0, len(args))
for _, t := range args {
	tag, err := l.ReadTag(t)
	if err != nil {
		entries = append(entries, render.StatusEntry{Target: t, Free: true})
	} else {
		entries = append(entries, render.StatusEntry{Target: t, Free: false, AgentID: tag.AgentID, Intent: tag.Intent})
	}
}
emitStatusTargets(entries)
return nil

// whoamiCmd RunE — replace `printJSON(a)`
emitWhoami(a)
return nil

// inboxCmd RunE — replace `printJSON(msgs)`. msgs is []loto.Msg.
out := make([]render.InboxMessage, len(msgs))
for i, m := range msgs {
	out[i] = render.InboxMessage{From: m.From, To: m.To, Body: m.Body}
}
emitInbox(args[0], out)
return nil

// msgCmd RunE — replace `printJSON(map[string]any{"sent": true, ...})`
emitMsgSent(args[0], to)
return nil

// releaseCmd RunE — replace `printJSON(map[string]any{...})`. Keep the slice
// (released []string) so the JSON branch preserves the existing shape.
emitReleased(agent, released, errsToStrings(errs))
if len(errs) > 0 { os.Exit(1) }
return nil

// reapCmd RunE — replace `printJSON(map[string]any{"reaped": true, ...})`
emitReaped(args[0])
return nil

// breakCmd force branch — replace `printJSON(...)` for force=true
emitBroken(target, by, reason)
// breakCmd reap branch
emitReaped(target)

// installHookCmd RunE — replace `printJSON(...)`
emitInstalled(".claude/settings.json")
return nil
```

Add the small wrappers at the bottom:

```go
func emitStatusGlobal(free bool, agent, intent string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMStatusGlobal(os.Stdout, free, agent, intent)
		return
	}
	if free {
		_ = render.EmitJSON(os.Stdout, map[string]any{"global": "free"})
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"global": map[string]any{"agent_id": agent, "intent": intent}})
}

func emitStatusTargets(entries []render.StatusEntry) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMStatusTargets(os.Stdout, entries)
		return
	}
	out := make(map[string]any, len(entries))
	for _, e := range entries {
		if e.Free {
			out[e.Target] = "free"
		} else {
			out[e.Target] = map[string]any{"agent_id": e.AgentID, "intent": e.Intent}
		}
	}
	_ = render.EmitJSON(os.Stdout, out)
}

func emitInbox(target string, msgs []render.InboxMessage) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMInbox(os.Stdout, target, msgs)
		return
	}
	_ = render.EmitJSON(os.Stdout, msgs)
}

func emitMsgSent(target, to string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMMsgSent(os.Stdout, target, to)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"sent": true, "to": to, "target": target})
}

func emitReleased(agent string, released []string, errs []string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMReleased(os.Stdout, agent, len(released), errs)
		return
	}
	// Preserve back-compat shape: released is a []string of paths.
	_ = render.EmitJSON(os.Stdout, map[string]any{"agent": agent, "released": released, "errors": errs})
}

func emitReaped(target string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMReaped(os.Stdout, target)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"reaped": true, "target": target})
}

func emitBroken(target, by, reason string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMBroken(os.Stdout, target, by, reason)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"broken": true, "force": true, "target": target, "by": by, "reason": reason})
}

func emitInstalled(path string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMInstalled(os.Stdout, path)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"installed": true, "file": path})
}
```

‡ **Note for the engineer:** The `releaseCmd` JSON shape currently emits `released: <slice>`, not a count. Audit the call site: if downstream consumers (project hook scripts) read the slice, the JSON branch above changes that to a count, breaking them. Verify by `rg "released" .claude/ ~/.claude/` before flipping. If the slice is depended on, keep the original JSON map and only change the LLM branch.

- [ ] **Step 4: Update `exit()` to render `ErrHeld` in the chosen format**

Replace the `exit` function:

```go
func exit(err error) {
	var sys *loto.ErrSystem
	if errors.As(err, &sys) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	var held *loto.ErrHeld
	if errors.As(err, &held) {
		if currentFormat == render.FormatLLM {
			in := render.BlockedInput{
				Kind: held.Kind, Target: held.Target,
			}
			if held.Tag != nil {
				in.AgentID = held.Tag.AgentID
				in.Intent = held.Tag.Intent
				in.HeldSince = held.Tag.Timestamp
				in.ExpiresAt = held.Tag.ExpiresAt
				in.Branch = held.Tag.Branch
				in.Host = held.Tag.Host
				in.PID = held.Tag.PID
			}
			_ = render.EmitLLMBlocked(os.Stderr, in)
			os.Exit(1)
		}
		// JSON path keeps the existing MarshalJSON behaviour.
		_ = render.EmitJSON(os.Stderr, held)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
```

- [ ] **Step 5: Build + run existing test suite**

```bash
cd /Users/vcto/Projects/loto && go build ./... && go test ./...
```

Expected: PASS. If any existing test pinned the JSON-as-default for non-tty, fix the test by passing `--json` explicitly, since the new default is LLM. Document each such fix in the commit message.

- [ ] **Step 6: Smoke-test the binary**

```bash
cd /Users/vcto/Projects/loto && go build -o /tmp/loto-llm ./cmd/loto
/tmp/loto-llm whoami | head -3              # expect llm format (piped = no tty)
/tmp/loto-llm whoami --json | head -3       # expect json
/tmp/loto-llm whoami --format=llm | head -3 # expect llm
/tmp/loto-llm status | head -3              # expect llm
```

Expected: piped runs emit `loto:llm:v1` header; `--json` emits JSON.

- [ ] **Step 7: Commit**

```bash
cd /Users/vcto/Projects/loto && git add cmd/loto/main.go
git commit -m "feat(cli): wire render package; default to llm when stdout is not a tty"
```

---

### Task 7: Integration test — pre-existing `--json` flag still works

The deployed project hooks use `--json`. Verify nothing breaks.

**Files:**
- Modify: `/Users/vcto/Projects/loto/cmd/loto/integration_test.go`

- [ ] **Step 1: Find the existing whoami integration assertion and add a `--json` case + a piped (default-llm) case**

Read the file first:

```bash
cd /Users/vcto/Projects/loto && rg -n "whoami" cmd/loto/integration_test.go
```

- [ ] **Step 2: Add a test that runs `loto whoami` (piped → llm) and checks for `loto:llm:v1`, plus `loto whoami --json` checks for valid JSON with an `id` field**

The file already defines `buildLotoBinary()` (line 34) returning `(path, error)` and a package-level `lotoBin` variable. Use those directly:

```go
// /Users/vcto/Projects/loto/cmd/loto/integration_test.go — append a test
// (existing imports already cover "encoding/json", "os/exec", "strings", "testing")

func TestIntegrationWhoamiFormatDefaults(t *testing.T) {
	if lotoBin == "" {
		t.Skip("loto binary not built")
	}
	out, err := exec.Command(lotoBin, "whoami").CombinedOutput()
	if err != nil {
		t.Fatalf("whoami: %v\n%s", err, out)
	}
	if !strings.HasPrefix(string(out), "loto:llm:v1\n") {
		t.Fatalf("expected llm header on piped stdout, got:\n%s", out)
	}
	out2, err := exec.Command(lotoBin, "whoami", "--json").CombinedOutput()
	if err != nil {
		t.Fatalf("whoami --json: %v\n%s", err, out2)
	}
	var got map[string]any
	if err := json.Unmarshal(out2, &got); err != nil {
		t.Fatalf("--json output not valid JSON: %v\n%s", err, out2)
	}
	if got["id"] == nil {
		t.Fatalf("--json missing id field: %s", out2)
	}
}
```

- [ ] **Step 3: Run**

```bash
cd /Users/vcto/Projects/loto && go test ./cmd/loto/ -run TestIntegrationWhoamiFormatDefaults
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
cd /Users/vcto/Projects/loto && git add cmd/loto/integration_test.go
git commit -m "test(cli): cover --json back-compat and llm-by-default for whoami"
```

---

## Phase 2 — Lift Hooks to Global Settings

### Task 8: Promote SessionStart + Stop hooks to `~/.claude/settings.json`

The current project-only hooks are at `/Users/vcto/Projects/loto/.claude/settings.json`. Lifting them globally means **every** Claude session (any project) gets a loto identity and auto-release on stop. Loto must be on PATH; the SessionStart command exits 0 silently when loto is missing (already does — `|| true` at the end of the pipe).

**Files:**
- Modify: `~/.claude/settings.json`

- [ ] **Step 1: Inspect current global settings**

```bash
cat ~/.claude/settings.json 2>/dev/null || echo '{}'
```

- [ ] **Step 2: Verify loto is on PATH for fresh shells**

```bash
which loto
# If absent: install with `make install` from /Users/vcto/Projects/loto, or symlink to /usr/local/bin.
```

Expected: a path. If not, install before continuing — the global hook depends on this.

- [ ] **Step 3: Edit `~/.claude/settings.json` by hand**

`loto install-hook` writes to the cwd's `.claude/settings.json` (project scope only — verified in `cmd/loto/main.go:writeClaudeHooks`). There is no `--global` flag today; adding one is out of scope for this plan. Edit `~/.claude/settings.json` directly (or via the update-config skill). Add (or merge) the hooks block:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "loto whoami --ensure --json | python3 -c \"import sys,json,os; d=json.load(sys.stdin); print('LOTO_AGENT_ID='+d['id'])\" >> $CLAUDE_ENV 2>/dev/null || true"
          }
        ]
      }
    ],
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "loto release --all-mine --json >/dev/null 2>&1 || true"
          }
        ]
      }
    ]
  }
}
```

‡ **Use `--json` explicitly here** even though the hook ignores stdout — it future-proofs against parser shape drift if loto's LLM format ever changes.

‡ **Merge, don't overwrite.** If `~/.claude/settings.json` has other hooks, append to the existing `hooks` block. Use `jq` if needed:

```bash
jq -s '.[0] * .[1]' ~/.claude/settings.json /tmp/loto-hooks.json > ~/.claude/settings.json.new && mv ~/.claude/settings.json.new ~/.claude/settings.json
```

- [ ] **Step 4: Verify in a new Claude session**

Open a new Claude Code session (any project, or a temp dir) and run:

```bash
echo "$LOTO_AGENT_ID"
loto whoami
```

Expected: a non-empty `LOTO_AGENT_ID` and a whoami matching that id.

- [ ] **Step 5: Decide fate of project-level hooks**

The project `loto/.claude/settings.json` now duplicates the global. Two options — pick one and document in the commit:

- **Keep both** (defensive — survives accidental global-settings deletion). The project hook will run *in addition*; SessionStart is idempotent (`whoami --ensure`), Stop too (`--all-mine` is a no-op when nothing's held).
- **Remove the project block** (single source of truth). Edit `loto/.claude/settings.json` to drop the hooks key.

Recommendation: keep both for now; revisit after a week of use.

- [ ] **Step 6: Commit (the global-settings change is outside the repo, so commit only project-side changes if any)**

```bash
# only if you changed loto/.claude/settings.json:
cd /Users/vcto/Projects/loto && git add .claude/settings.json
git commit -m "chore(hooks): drop duplicate project hook (global takes over)"
```

---

## Phase 3 — Write the Loto Skill

### Task 9: Author `~/.claude/skills/loto/SKILL.md`

Skills live in `~/.claude/skills/<name>/SKILL.md`. The skill teaches the operating loop using `--llm` output and explains when to invoke loto.

**Files:**
- Create: `~/.claude/skills/loto/SKILL.md`

- [ ] **Step 1: Confirm the skill directory layout**

```bash
ls ~/.claude/skills 2>/dev/null | head -20
```

If skills are stored elsewhere (e.g. plugin path), match the existing layout. The frontmatter `name` and `description` are what the skill loader indexes.

- [ ] **Step 2: Write the skill**

Create `~/.claude/skills/loto/SKILL.md` with this exact content:

````markdown
---
name: loto
description: Use when about to edit a file in a project where multiple Claude sessions may be running (worktrees, subagents, concurrent windows), or before any large refactor that touches many files. Coordinates file/global locks to prevent silent clobbers. Triggers: "edit X", "refactor Y", "modify Z" in shared repos; "I'm starting a sweep across …"; conflict-shaped errors after an edit; "who has X locked?"; "what am I called in this project?".
---

# loto — multi-agent file coordination

‡ **Default output is LLM-format** (terse, `loto:llm:v1` header). Pass `--json` only when piping to `jq` or scripts that parsed the legacy shape.

## When to use

- Any time you're about to edit a file, *and* you suspect another Claude session may be active in the same repo (worktrees, named subagents, multiple windows).
- Before a multi-file refactor: stake a glob reservation.
- When you see surprising diffs ("I didn't write that") — run `loto status` to find out who did.

## Operating loop

```
1. orient    → loto whoami
2. intend    → loto reserve "<glob>"      # optional
3. acquire   → loto try file <path>       # exit 0 + acquired line, or exit 1 + blocked line
4. edit      → ... do the work ...
5. read msgs → loto inbox <path>
6. release   → loto release --all-mine    # explicit, or auto-on-session-end
```

## Reading LLM output

Format: first line `loto:llm:v1`, body lines use `|`-separated fields with leading severity glyph.

| Glyph | Meaning |
|-------|---------|
| `✔`   | success |
| `✗`   | conflict / error |
| `⚠`   | warning (e.g. reservation overlap) |
| `→`   | message / row continuation |

### Examples

**whoami:**
```
loto:llm:v1
agent | RemoteSnipe | id:2dd46381 | host:Mac
```

**try file (success):**
```
loto:llm:v1
✔ acquired | file | internal/store/store.go | by:GreenCastle
```

**try file (blocked, on stderr, exit 1):**
```
loto:llm:v1
✗ blocked | file | internal/store/store.go | by:BlueOak | intent:store refactor — see beads loto-7wp.4 | held-since:2026-04-28T14:32:11Z | ttl:2026-04-28T14:42:11Z | branch:store-refactor | host:dk-mac | pid:84231
```

When blocked, you have three actions:
1. **Wait** — `loto try file <path> --wait 30s`.
2. **Work elsewhere** — pick another file or task.
3. **Message the holder** — `loto msg <path> --to <agent> "need 5min on this"`.

**status (per-target table):**
```
loto:llm:v1
status | target | holder | intent
✔ free | a.go | - | -
✗ held | b.go | GreenCastle | store refactor
```

## Exit codes

| Code | Meaning |
|------|---------|
| 0    | success |
| 1    | advisory conflict (someone holds it) |
| 2    | usage error |
| 3    | system / IO error |

## Don'ts

- ✗ Use `--no-verify` to bypass the loto pre-commit hook. If it fires, someone else is holding what you're committing — talk to them first.
- ✗ `loto break --force` without a `--reason`. The displaced agent gets a mailbox message; give them the why.
- ✗ Hold a file lock across long-running tool calls (builds, tests). Acquire just before the edit, release just after.

## Cross-refs

- `~/Projects/loto/docs/NORTH_STAR.md` — full design rationale
- nug `32f0ece29b72` — Claude-Optimized Utility Output standard (the format)
- nug `c75320ff5718` — Symbol Glossary (the glyphs)
````

- [ ] **Step 3: Verify the skill loads in a fresh Claude session**

Open a new Claude Code session and check:

```
/help skills | grep loto
```

(Or whatever the platform's skill listing is — see `~/.claude/skills/using-superpowers` reference for the listing path.)

Expected: `loto` appears with the description.

- [ ] **Step 4: Sanity-test triggering**

In a fresh session, ask: "I want to edit a file in this repo — anything I should do first?"

Expected: Claude invokes the loto skill and walks through `whoami → try → edit → release`.

- [ ] **Step 5: Commit (skill is outside the repo; nothing to commit unless you ALSO copy it into this repo's docs/)**

Optional — copy a snapshot into `docs/skills/loto.md` for visibility, then:

```bash
cd /Users/vcto/Projects/loto && mkdir -p docs/skills && cp ~/.claude/skills/loto/SKILL.md docs/skills/loto.md
git add docs/skills/loto.md
git commit -m "docs(skills): snapshot the global loto skill for in-repo visibility"
```

---

## Phase 4 — Documentation Update

### Task 10: Update `README.md` to mention `--llm` and the skill

**Files:**
- Modify: `/Users/vcto/Projects/loto/README.md`

- [ ] **Step 1: Read current README quick-start section**

```bash
sed -n '30,80p' /Users/vcto/Projects/loto/README.md
```

- [ ] **Step 2: Update the quick-start to show LLM output and call out `--json` as opt-in**

Replace the JSON example lines with:

```markdown
## quick start

```sh
# Who am I in this session?
loto whoami
# → loto:llm:v1
# → agent | RemoteSnipe | id:2dd46381 | host:Mac

# Acquire a file lock (non-blocking):
loto try file internal/store/store.go
# → loto:llm:v1
# → ✔ acquired | file | internal/store/store.go | by:RemoteSnipe

# When blocked (stderr, exit 1):
# → ✗ blocked | file | … | by:GreenCastle | intent:… | held-since:…

# JSON output for scripts / hooks:
loto whoami --json
```

By default, loto emits the **claude-optimized** terse format when stdout is
not a tty (≈40-60% fewer tokens than JSON; see the Claude-Optimized Utility
Output standard, nug `32f0ece29b72`). Pipe consumers and existing hooks
should pass `--json` explicitly.
```

- [ ] **Step 3: Add a one-liner pointing at the skill**

Near the top of the README, after the `## what it does` paragraph, add:

```markdown
> Using Claude Code? Install the loto skill at `~/.claude/skills/loto/SKILL.md`
> (see `docs/skills/loto.md`) and the global hooks at `~/.claude/settings.json`
> so every session gets identity + auto-release.
```

- [ ] **Step 4: Commit**

```bash
cd /Users/vcto/Projects/loto && git add README.md
git commit -m "docs(readme): document --llm default and skill/hooks setup"
```

---

## Verification

- [ ] `go build ./... && go test ./...` — clean
- [ ] `loto whoami | head -1` shows `loto:llm:v1`
- [ ] `loto whoami --json` shows JSON with `id` field
- [ ] `loto try file /tmp/x` (held by another shell) emits `✗ blocked …` to stderr, exit 1
- [ ] New Claude session: `echo $LOTO_AGENT_ID` non-empty
- [ ] New Claude session: skill `loto` appears and triggers on edit-related prompts
- [ ] Existing project hooks (`loto/.claude/settings.json`) still work — they use `--json` explicitly

---

## Out of scope (do not do in this plan)

- Switching tty default from JSON to LLM (keeps human-facing behaviour stable; revisit after the format is validated in agent loops).
- Adding a third `human` format (only relevant once tty default is reconsidered).
- An MCP server exposing loto as native tools (separate, larger epic).
- Doctor command LLM rendering (low traffic; do later).
- Reservation listing LLM rendering (`loto reserve --list` — same).
