package render

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestEmitGateDeny_ByteExact pins the exact `loto check --gate` deny-block
// shape (loto-vr2, gate-design.md component 4): count-first header, rows
// sorted path -> kind -> holder, a claim-kind row carries the options line
// (no takeover verb exists — deliberate) while a lock-kind row carries the
// existing `loto unlock --force` fix block mirroring printCheckConflicts.
func TestEmitGateDeny_ByteExact(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // empty registry -> holderTag falls back to bare UUID
	expires := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := []GateDenyRow{
		{
			Path: bGo, Kind: GateKindLock, HolderUUID: "foe-uuid",
			Intent: "editing", ExpiresAt: expires, BlockerPath: bGo,
		},
		{
			Path: "internal/store/new.go", Kind: GateKindClaim, HolderUUID: "foe-uuid",
			Intent: "gate-intent", ExpiresAt: expires, BlockerPath: "internal/store",
		},
	}
	var buf bytes.Buffer
	EmitGateDeny(&buf, rows)
	want := "✗ blocked count=2\n" +
		"✗ path=b.go kind=lock blocker=foe-uuid intent=\"editing\" expires_at=2026-01-02T03:04:05Z\n" +
		"```bash\n" +
		"loto unlock --force -t \"unblock\" b.go\n" +
		"```\n" +
		"✗ path=internal/store/new.go kind=claim blocker=foe-uuid prefix=internal/store intent=\"gate-intent\" expires_at=2026-01-02T03:04:05Z\n" +
		"ℹ options=wait|pick-other-work|message-holder\n"
	if got := buf.String(); got != want {
		t.Errorf("EmitGateDeny mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmitGateDeny_SortsPathThenKindThenHolder pins the deterministic sort
// contract independent of input order.
func TestEmitGateDeny_SortsPathThenKindThenHolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	expires := time.Now().Add(time.Hour)
	rows := []GateDenyRow{
		{Path: "zzz.go", Kind: GateKindLock, HolderUUID: "zzz", ExpiresAt: expires, BlockerPath: "zzz.go"},
		{Path: aGo, Kind: GateKindClaim, HolderUUID: "bbb", ExpiresAt: expires, BlockerPath: "."},
		{Path: aGo, Kind: GateKindClaim, HolderUUID: "aaa", ExpiresAt: expires, BlockerPath: "."},
	}
	var buf bytes.Buffer
	EmitGateDeny(&buf, rows)
	got := buf.String()
	wantOrder := []string{
		"path=a.go kind=claim blocker=aaa",
		"path=a.go kind=claim blocker=bbb",
		"path=zzz.go kind=lock blocker=zzz",
	}
	lastIdx := -1
	for _, w := range wantOrder {
		idx := strings.Index(got, w)
		if idx == -1 {
			t.Fatalf("missing row %q in output: %q", w, got)
		}
		if idx < lastIdx {
			t.Fatalf("row %q out of order in output: %q", w, got)
		}
		lastIdx = idx
	}
}
