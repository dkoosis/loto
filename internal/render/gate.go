package render

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"time"
)

// GateKindLock names what denied a `loto check --gate` target (loto-vr2,
// gate-design.md component 4): a hold. Its row carries the same
// `loto unlock --force` fix line printCheckConflicts uses for a live blocker.
const GateKindLock = "lock"

// GateDenyRow is one `loto check --gate` deny: a foreign live hold covers
// Path. BlockerPath is the hold's own target: Path itself, or a directory
// above it (which may cover a path not yet on disk).
type GateDenyRow struct {
	Path        string
	Kind        string
	HolderUUID  string
	Intent      string
	ExpiresAt   time.Time
	BlockerPath string
}

// EmitGateDeny renders the `✗ blocked` block for `loto check --gate`. Rows
// are re-sorted path -> kind -> holder -> blocker path defensively
// (.claude/rules/design.md: same input, byte-identical output), independent
// of caller order; the blocker-path tie-break keeps two same-owner holds at
// different directories above one target in a stable order. The `blocker=`
// field name matches EmitConflictWithTags — one vocabulary for "who is
// blocking me" across every conflict-reporting surface. Each row but a root
// claim's carries the `loto unlock --force` fix block, mirroring
// printCheckConflicts, naming the hold's own target.
func EmitGateDeny(w io.Writer, rows []GateDenyRow) {
	cwd := getCwd()
	sorted := slices.Clone(rows)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		if sorted[i].Kind != sorted[j].Kind {
			return sorted[i].Kind < sorted[j].Kind
		}
		if sorted[i].HolderUUID != sorted[j].HolderUUID {
			return sorted[i].HolderUUID < sorted[j].HolderUUID
		}
		return sorted[i].BlockerPath < sorted[j].BlockerPath
	})
	fmt.Fprintf(w, "✗ blocked count=%d\n", len(sorted))
	for i := range sorted {
		r := &sorted[i]
		fmt.Fprintf(w, "✗ path=%s kind=%s blocker=%s intent=%q expires_at=%s\n",
			relToCwd(r.Path, cwd), r.Kind, holderTag(r.HolderUUID), r.Intent,
			r.ExpiresAt.UTC().Format(time.RFC3339))
		if r.BlockerPath == "." {
			continue // unlock refuses the repo root, so a root claim has no fix to print
		}
		fmt.Fprintln(w, "```bash")
		fmt.Fprintf(w, "loto unlock --force -t \"unblock\" %s\n", relToCwd(r.BlockerPath, cwd))
		fmt.Fprintln(w, "```")
	}
}
