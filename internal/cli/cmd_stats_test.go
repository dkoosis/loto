package cli

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"

	_ "modernc.org/sqlite"
)

const tcCmdStats = "stats"

// seedEnforcementEvent appends one event of kind through the real store the
// CLI reads, so `loto stats` exercises the same read path production does.
func seedEnforcementEvent(t *testing.T, kind, reason string) {
	t.Helper()
	rt, done := hookStoreRead(t)
	defer done()
	if _, err := rt.Store.AppendEvent(rt.Ctx, domain.Event{
		Kind: kind, ActorUUID: "seed", Reason: reason, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed %s event: %v", kind, err)
	}
}

// ageAllEvents rewrites every event row's created_at to `age` ago, directly
// in the sqlite file — the same direct-UPDATE trick forceLockExpiry uses
// (cmd_refresh_test.go) to exercise a window without a real time.Sleep.
func ageAllEvents(t *testing.T, age time.Duration) {
	t.Helper()
	base := os.Getenv("LOTO_BASE")
	if base == "" {
		t.Fatal("ageAllEvents: LOTO_BASE not set — call after withTempProject")
	}
	db, err := sql.Open("sqlite", filepath.Join(base, "loto.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("ageAllEvents: open store db directly: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE events SET created_at = ?`, time.Now().Add(-age).UnixNano()); err != nil {
		t.Fatalf("ageAllEvents: update: %v", err)
	}
}

func runStats(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := Run(append([]string{tcCmdStats}, args...), &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// A fresh repo prints all four §10b rows at zero/✓, the empty-store baseline.
func TestStats_FreshRepoIsAllPass(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	code, out, errOut := runStats(t)
	if code != 0 {
		t.Fatalf("exit=%d err=%q", code, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "✓ enforcement-stats since=24h0m0s ") {
		t.Errorf("triage line = %q", lines[0])
	}
}

// --since accepts a day count, per this bead's AC and cmd_stats.go's sinceFlag.
func TestStats_SinceAcceptsDaySuffix(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	for _, since := range []string{"30d", "7d"} {
		code, out, errOut := runStats(t, "--since", since)
		if code != 0 {
			t.Fatalf("--since %s: exit=%d err=%q", since, code, errOut)
		}
		if !strings.Contains(out, "since="+sinceGoDurationOf(t, since)) {
			t.Errorf("--since %s: triage line did not echo the parsed window: %q", since, out)
		}
	}
}

// sinceGoDurationOf is the Go-duration spelling a day count parses to, for
// asserting the rendered "since=" field.
func sinceGoDurationOf(t *testing.T, dayCount string) string {
	t.Helper()
	n := strings.TrimSuffix(dayCount, "d")
	switch n {
	case "30":
		return (30 * 24 * time.Hour).String()
	case "7":
		return (7 * 24 * time.Hour).String()
	default:
		t.Fatalf("unhandled day count %q in test fixture", dayCount)
		return ""
	}
}

// --since excludes rows older than the window: a row aged past 7 days does
// not show up under --since 7d.
func TestStats_SinceExcludesOlderRows(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	seedEnforcementEvent(t, "ref_refused", "stash")
	ageAllEvents(t, 8*24*time.Hour)

	code, out, errOut := runStats(t, "--since", "7d")
	if code != 0 {
		t.Fatalf("exit=%d err=%q", code, errOut)
	}
	if !strings.Contains(out, "ref_refused=0") {
		t.Errorf("a row aged past --since 7d was still counted: %q", out)
	}

	code, out, errOut = runStats(t, "--since", "30d")
	if code != 0 {
		t.Fatalf("exit=%d err=%q", code, errOut)
	}
	if !strings.Contains(out, "ref_refused=1") {
		t.Errorf("--since 30d should still see an 8-day-old row: %q", out)
	}
}

// doctor's first line repeats stats' first line (this bead's Rules) — the
// enforcement-stats triage line renders identically from both commands, on
// the same store.
func TestDoctorFirstLineEqualsStatsFirstLine(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	seedEnforcementEvent(t, "ref_refused", "stash")

	_, statsOut, errOut := runStats(t)
	if statsOut == "" {
		t.Fatalf("empty stats output, err=%q", errOut)
	}
	statsFirst := strings.SplitN(statsOut, "\n", 2)[0]
	if !strings.HasPrefix(statsFirst, "enforcement-stats") && !strings.Contains(statsFirst, "enforcement-stats") {
		t.Fatalf("stats' first line does not look like the triage line: %q", statsFirst)
	}

	var doctorOut bytes.Buffer
	if code := Run([]string{tcCmdDoctor}, &doctorOut, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doctor exit=%d", code)
	}
	var doctorEnforcementLine string
	for l := range strings.SplitSeq(doctorOut.String(), "\n") {
		if strings.Contains(l, "enforcement-stats") {
			doctorEnforcementLine = l
			break
		}
	}
	if doctorEnforcementLine == "" {
		t.Fatalf("doctor printed no enforcement-stats line:\n%s", doctorOut.String())
	}
	if doctorEnforcementLine != statsFirst {
		t.Errorf("doctor's enforcement-stats line != stats' first line:\n doctor %q\n stats  %q", doctorEnforcementLine, statsFirst)
	}
}
