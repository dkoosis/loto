package cli

import "testing"

// The porcelain shape git actually emits, captured from a real repo with one
// primary checkout and one locked linked worktree.
const tcPorcelain = `worktree /Users/x/Projects/loto
HEAD de7baaeb6d47902562df6c9c520246481f59743f
branch refs/heads/main

worktree /Users/x/Projects/loto/.claude/worktrees/agent-a88
HEAD dd26e77307f98e319323e34e2feb0296963c017d
branch refs/heads/loto-ovno.11-gate-contract-tests
locked claude agent agent-a8837e01287e0045d (pid 44312 start Thu Aug 20 19:21:35 2026)
`

func TestParseWorktreePorcelain_ReadsBothCheckouts(t *testing.T) {
	recs := parseWorktreePorcelain(tcPorcelain)
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d: %+v", len(recs), recs)
	}
	if recs[0].Branch != "refs/heads/main" || recs[0].Locked {
		t.Errorf("primary record wrong: %+v", recs[0])
	}
	if !recs[1].Locked || recs[1].Branch != "refs/heads/loto-ovno.11-gate-contract-tests" {
		t.Errorf("linked record wrong: %+v", recs[1])
	}
}

// A trailing record with no blank line after it must still be emitted — git
// does not always end its output with one.
func TestParseWorktreePorcelain_NoTrailingBlankLine(t *testing.T) {
	recs := parseWorktreePorcelain("worktree /a\nHEAD abc\ndetached")
	if len(recs) != 1 || recs[0].Path != "/a" {
		t.Fatalf("trailing record dropped: %+v", recs)
	}
}

// An attribute this binary has never met is ignored, not refused. git keeps
// adding them, and a parser that failed on one would turn a routine git
// upgrade into a repo-wide refusal to publish anything.
func TestParseWorktreePorcelain_UnknownAttributeIsIgnored(t *testing.T) {
	recs := parseWorktreePorcelain("worktree /a\nbranch refs/heads/b\nprunable gitdir file points to non-existent location\n")
	if len(recs) != 1 || recs[0].Branch != "refs/heads/b" {
		t.Fatalf("unknown attribute broke the record: %+v", recs)
	}
}

// `locked` with no reason is still a lock.
func TestParseWorktreePorcelain_BareLockIsALock(t *testing.T) {
	recs := parseWorktreePorcelain("worktree /a\nbranch refs/heads/b\nlocked\n")
	if len(recs) != 1 || !recs[0].Locked || recs[0].Reason != "" {
		t.Fatalf("bare lock misread: %+v", recs)
	}
}

func TestLockReasonPID(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   int
		wantOK bool
	}{
		// The token arrives punctuated: "(pid 44312". Reading only a bare
		// "pid" made a real, perfectly readable lock report as unreadable.
		{"claude code's own wording", "claude agent agent-a88 (pid 44312 start Thu Aug 20 19:21:35 2026)", 44312, true},
		{"unpunctuated", "held by pid 7 forever", 7, true},
		{"no pid at all", "manual lock, do not touch", 0, false},
		{"pid is the last word", "locked by pid", 0, false},
		{"not a number", "pid unknown", 0, false},
		{"zero is not a pid", "(pid 0)", 0, false},
		{"empty reason", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := lockReasonPID(tt.reason)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("lockReasonPID(%q) = (%d, %v), want (%d, %v)", tt.reason, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
