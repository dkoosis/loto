package render

import (
	"strings"
	"testing"
	"time"
)

// tvUnleased is the lease state every row in this file is observed under —
// what a violation IS. Named so the report assertions read as prose.
const tvUnleased = "unleased"

// The primary checkout's report is byte-for-byte what it was before rows
// carried a worktree: a field that never varies is noise on every line.
func TestEmitViolations_PrimaryCheckoutCarriesNoWorktreeField(t *testing.T) {
	var b strings.Builder
	EmitViolations(&b, []ViolationRow{{
		ID: "v-1", Path: aGo, LeaseState: tvUnleased, ObservedAt: time.Unix(0, 0),
	}})
	if strings.Contains(b.String(), "worktree=") {
		t.Errorf("primary row carries a worktree field: %q", b.String())
	}
}

// Two checkouts can each hold an open row for one path. Without the name on
// the row the operator cannot tell which tree to go clean (loto-nper).
func TestEmitViolations_NamesTheCheckoutWhenRowsShareAPath(t *testing.T) {
	var b strings.Builder
	EmitViolations(&b, []ViolationRow{
		{ID: "v-2", Path: aGo, LeaseState: tvUnleased, ObservedAt: time.Unix(0, 0), Worktree: "agent-b"},
		{ID: "v-1", Path: aGo, LeaseState: tvUnleased, ObservedAt: time.Unix(0, 0)},
	})
	out := b.String()
	if !strings.Contains(out, "path="+aGo+" id=v-1 state=unleased observed=1970-01-01T00:00:00Z\n") {
		t.Errorf("primary row wrong: %q", out)
	}
	if !strings.Contains(out, "path="+aGo+" id=v-2 state=unleased observed=1970-01-01T00:00:00Z worktree=agent-b\n") {
		t.Errorf("linked row missing its checkout: %q", out)
	}
	// Deterministic: primary ("") sorts ahead of agent-b on a shared path.
	if strings.Index(out, "id=v-1") > strings.Index(out, "id=v-2") {
		t.Errorf("rows not sorted by worktree within a path: %q", out)
	}
}
