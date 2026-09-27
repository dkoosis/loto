package render

import (
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

// trOwner1 is the one owner every fixture in this file uses — both as
// addressee and as the event's holder/observer, since these tests are about
// the worktree field, not about who reported to whom.
const trOwner1 = "owner-1"

// TestEmitReports_ControlCharWorktreeStaysOneLine is Codex P2 on PR #377
// (loto-1h6x): a linked worktree's directory name is whatever the filesystem
// allows, including an ASCII control character. Printed verbatim it would
// split the one-row-per-line report the PreToolUse hook and `loto status`
// are parsed from; escapeRowField's %q-quoting (the rowPath precedent,
// cmd_check_held.go) keeps the row to one line.
func TestEmitReports_ControlCharWorktreeStaysOneLine(t *testing.T) {
	evilWorktree := "/tmp/wt\nrogue"
	if !domain.HasControl(evilWorktree) {
		t.Fatalf("fixture must carry a control character: %q", evilWorktree)
	}
	report := store.TreeReport{
		ReportID:  "r-1",
		Addressee: trOwner1,
		CreatedAt: time.Unix(0, 0),
		Event: store.TreeEvent{
			EventID:    "e-1",
			Path:       "a.go",
			HolderPre:  trOwner1,
			Observer:   trOwner1,
			CallID:     "call-1",
			Seq:        1,
			DigestPre:  "d0",
			DigestPost: "d1",
			Rule:       store.TreeRuleRow2,
			Note:       "changed in your call, no competing call seen",
			CreatedAt:  time.Unix(0, 0),
			Worktree:   evilWorktree,
		},
	}

	var out strings.Builder
	EmitReports(&out, "reports", []store.TreeReport{report}, false, "/tmp/other-worktree")

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	// header, one report row, ```bash, the command, ``` — five lines. A
	// control character reaching output unescaped would split the report row
	// into two, making this six.
	if len(lines) != 5 {
		t.Fatalf("want 5 lines, got %d: %q", len(lines), out.String())
	}
	if !strings.HasPrefix(lines[1], "⚠ path=a.go worktree=") {
		t.Fatalf("the report row must still be one line naming the worktree: %q", lines[1])
	}
	if strings.Contains(out.String(), evilWorktree) {
		t.Errorf("the raw control character must not reach output unescaped: %q", out.String())
	}
}

// TestEmitReports_OrdinaryWorktreeUnquoted pins the golden diff: an ordinary
// worktree path (the common case) is printed exactly as before this fix, no
// quoting.
func TestEmitReports_OrdinaryWorktreeUnquoted(t *testing.T) {
	report := store.TreeReport{
		ReportID:  "r-1",
		Addressee: trOwner1,
		CreatedAt: time.Unix(0, 0),
		Event: store.TreeEvent{
			EventID:    "e-1",
			Path:       "a.go",
			HolderPre:  trOwner1,
			Observer:   trOwner1,
			CallID:     "call-1",
			Seq:        1,
			DigestPre:  "d0",
			DigestPost: "d1",
			Rule:       store.TreeRuleRow2,
			Note:       "changed in your call, no competing call seen",
			CreatedAt:  time.Unix(0, 0),
			Worktree:   "/tmp/wt-b",
		},
	}

	var out strings.Builder
	EmitReports(&out, "reports", []store.TreeReport{report}, false, "/tmp/wt-a")

	if !strings.Contains(out.String(), "worktree=/tmp/wt-b ") {
		t.Errorf("an ordinary worktree path must render unquoted: %q", out.String())
	}
}
