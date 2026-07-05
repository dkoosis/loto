package render

import (
	"fmt"
	"io"
	"sort"
	"time"
)

// GateKindClaim/GateKindLock name what denied a `loto check --gate` target
// (loto-vr2, gate-design.md component 4). A claim-kind row has no takeover
// verb — the only remedies are wait/pick-other-work/message-holder. A
// lock-kind row (covers both exclusive locks and shared beacons — a beacon
// is a ModeShared domain.LockRecord) carries the same `loto unlock --force`
// fix line printCheckConflicts uses for a live blocker.
const (
	GateKindClaim = "claim"
	GateKindLock  = "lock"
)

// GateDenyRow is one `loto check --gate` deny: a foreign live claim or
// lock/beacon covers Path. BlockerPath is the blocker's own canonical path —
// a claim's PathPrefix (may be an ancestor of Path, not equal to it: the
// kuv.10 "not yet on disk" class) or a lock's Target.Canonical (always ==
// Path, since locks are per-file).
type GateDenyRow struct {
	Path        string
	Kind        string
	HolderUUID  string
	Intent      string
	ExpiresAt   time.Time
	BlockerPath string
}

// EmitGateDeny renders the `✗ blocked` block for `loto check --gate`. Rows
// are re-sorted path -> kind -> holder defensively (.claude/rules/design.md:
// same input, byte-identical output), independent of caller order. The
// `blocker=` field name matches EmitClaimConflict/EmitConflictWithTags — one
// vocabulary for "who is blocking me" across every conflict-reporting
// surface. A claim-kind row also carries `prefix=` (the claimed ancestor
// territory — may differ from path, the kuv.10 "not yet on disk" class) and
// no fix command — no takeover verb exists — just the options line. A
// lock-kind row carries the existing `loto unlock --force` fix block,
// mirroring printCheckConflicts; its blocker path always equals path (locks
// are per-file), so it isn't repeated as a separate field.
func EmitGateDeny(w io.Writer, rows []GateDenyRow) {
	cwd := getCwd()
	holders := &holderMemo{}
	sorted := append([]GateDenyRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		if sorted[i].Kind != sorted[j].Kind {
			return sorted[i].Kind < sorted[j].Kind
		}
		return sorted[i].HolderUUID < sorted[j].HolderUUID
	})
	fmt.Fprintf(w, "✗ blocked count=%d\n", len(sorted))
	for i := range sorted {
		r := &sorted[i]
		if r.Kind == GateKindClaim {
			fmt.Fprintf(w, "✗ path=%s kind=%s blocker=%s prefix=%s intent=%q expires_at=%s\n",
				relToCwd(r.Path, cwd), r.Kind, holders.tag(r.HolderUUID), relToCwd(r.BlockerPath, cwd),
				r.Intent, r.ExpiresAt.UTC().Format(time.RFC3339))
			fmt.Fprintln(w, "ℹ options=wait|pick-other-work|message-holder")
			continue
		}
		fmt.Fprintf(w, "✗ path=%s kind=%s blocker=%s intent=%q expires_at=%s\n",
			relToCwd(r.Path, cwd), r.Kind, holders.tag(r.HolderUUID), r.Intent,
			r.ExpiresAt.UTC().Format(time.RFC3339))
		fmt.Fprintln(w, "```bash")
		fmt.Fprintf(w, "loto unlock --force -t \"unblock\" %s\n", relToCwd(r.BlockerPath, cwd))
		fmt.Fprintln(w, "```")
	}
}
