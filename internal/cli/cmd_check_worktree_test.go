package cli

import (
	"testing"

	"loto/internal/identity"
)

func identityCanon(p string) string { return p } // test paths are already canonical

// TestDecideWorktreePresence pins the per-session rule decideWorktreePresence
// documents, for the inputs the txtar cannot stage: a loto record whose
// liveness cannot be decided, one session placed by both records, and a
// sibling path that shares the target's prefix (loto-nwoz).
func TestDecideWorktreePresence(t *testing.T) {
	const main, wa, wb = "/r/main", "/r/main/.claude/worktrees/wa", "/r/wb"
	trees := []string{main, wa, wb}
	rec := func(sid, checkout string, pid int) identity.SessionRecord {
		return identity.SessionRecord{SessionID: sid, Repo: main, Checkout: checkout, PID: pid}
	}

	cases := []struct {
		name   string
		target string
		loto   []lotoSession
		cc     []claudeSession
		fold   bool
		want   []presenceVerdict
	}{
		{
			name:   "unknown loto liveness anywhere in the repo is unknown",
			target: wb,
			loto:   []lotoSession{{Rec: rec("s1", main, 0), Liveness: identity.SessionUnknown, Reason: "no-witness"}},
			want:   []presenceVerdict{{Session: "s1", State: presenceUnknown, Source: presenceSourceLoto, At: main, Reason: "no-witness"}},
		},
		{
			name:   "unknown loto liveness in another repo is not this repo's business",
			target: wb,
			loto:   []lotoSession{{Rec: identity.SessionRecord{SessionID: "s1", Repo: "/elsewhere"}, Liveness: identity.SessionUnknown}},
		},
		{
			name:   "one session placed by both records is one row",
			target: wa,
			loto:   []lotoSession{{Rec: rec("s1", wa, 7), Liveness: identity.SessionLive}},
			cc:     []claudeSession{{Rec: identity.ClaudeSession{PID: 7, SessionID: "s1", CWD: wa}, Liveness: identity.SessionLive}},
			want:   []presenceVerdict{{Session: "s1", PID: 7, State: presenceOccupied, Source: presenceSourceLoto, At: wa}},
		},
		{
			name:   "Claude cwd inside the target counts even when loto places the session elsewhere",
			target: wb,
			loto:   []lotoSession{{Rec: rec("s1", main, 7), Liveness: identity.SessionLive}},
			cc:     []claudeSession{{Rec: identity.ClaudeSession{PID: 7, SessionID: "s1", CWD: wb + "/sub"}, Liveness: identity.SessionLive}},
			want:   []presenceVerdict{{Session: "s1", PID: 7, State: presenceOccupied, Source: presenceSourceClaude, At: wb + "/sub"}},
		},
		{
			name:   "a sibling sharing the prefix is not inside",
			target: wb,
			loto:   []lotoSession{{Rec: rec("s1", "/r/wbx", 7), Liveness: identity.SessionLive}},
		},
		{
			name:   "a case-folding filesystem matches a record spelled in another case",
			target: wb,
			fold:   true,
			loto:   []lotoSession{{Rec: rec("s1", "/R/WB", 7), Liveness: identity.SessionLive}},
			want:   []presenceVerdict{{Session: "s1", PID: 7, State: presenceOccupied, Source: presenceSourceLoto, At: "/R/WB"}},
		},
		{
			name:   "a case-sensitive filesystem does not",
			target: wb,
			loto:   []lotoSession{{Rec: rec("s1", "/R/WB", 7), Liveness: identity.SessionLive}},
		},
		{
			name:   "a dead Claude session is nothing",
			target: main,
			cc:     []claudeSession{{Rec: identity.ClaudeSession{PID: 9, SessionID: "c1", CWD: main}, Liveness: identity.SessionDead}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideWorktreePresence(presenceScope{Target: tc.target, Trees: trees, Fold: tc.fold}, tc.loto, tc.cc, identityCanon)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("row %d: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
