//go:build perf

package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"loto/internal/store"
)

// TestHook_PrePostMedianUnder50msAt300KB is loto-szdx's benchmark clause:
// with 300KB locked over 20 files, a pre+post pair's median stays under 50ms —
// doctor's hook_cost threshold (p50>50ms) reads the same pair. Measured
// in-process, as the hook_timing row is. A wall-clock bound depends on the
// host, so it builds only under the perf tag (`make perf`), never in `make
// check` or CI. Skipped under -race and -short, where the bound measures the
// instrumentation, not the hook.
func TestHook_PrePostMedianUnder50msAt300KB(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("wall-clock bound; -race and -short runs do not measure the hook")
	}
	repo := withTempProject(t)
	pinAgent(t)
	const files, fileBytes = 20, 15 * 1024
	body := bytes.Repeat([]byte("x"), fileBytes)
	lock := make([]string, 0, 1+files+2)
	lock = append(lock, tcCmdLock)
	for i := range files {
		name := fmt.Sprintf("locked-%02d.txt", i)
		if err := os.WriteFile(filepath.Join(repo, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		lock = append(lock, name)
	}
	lock = append(lock, tcFlagIntent, tcIntentTest)
	if code := Run(lock, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("seed locks: exit %d", code)
	}

	// Up to three batches; the first whose median is under the bound passes.
	// A load spike on the machine slows one batch, while a real regression
	// slows all three.
	const batches, pairs = 3, 21
	var medians []time.Duration
	for b := range batches {
		took := make([]time.Duration, 0, pairs)
		for i := range pairs {
			ev := hookEventJSON("Bash", fmt.Sprintf("cost-%d-%d", b, i), "", "")
			start := time.Now()
			if _, errOut, code := runHookEvent(t, tcHookPre, ev); code != 0 {
				t.Fatalf("pre %d-%d: exit=%d err=%q", b, i, code, errOut)
			}
			if _, errOut, code := runHookEvent(t, tcHookPost, ev); code != 0 {
				t.Fatalf("post %d-%d: exit=%d err=%q", b, i, code, errOut)
			}
			took = append(took, time.Since(start))
		}
		if b == 0 {
			hookCostSawLockedBytes(t, files*fileBytes)
		}
		slices.Sort(took)
		medians = append(medians, took[pairs/2])
		if took[pairs/2] < 50*time.Millisecond {
			return
		}
	}
	t.Errorf("pre+post p50 per batch = %v, want one < 50ms", medians)
}

// hookCostSawLockedBytes checks the pairs observed the scenario the clause
// names: the last hook_timing row counts every locked byte.
func hookCostSawLockedBytes(t *testing.T, want int) {
	t.Helper()
	var out bytes.Buffer
	if code := Run([]string{tcCmdEvents, tcFlagKind, store.EventHookTiming, "--limit", "1"}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("loto events: exit %d", code)
	}
	if row := fmt.Sprintf(`"locked_bytes":%d`, want); !strings.Contains(out.String(), row) {
		t.Fatalf("want %s in the last timing row: %q", row, out.String())
	}
}

// TestHook_EditDecidesUnder10msAt200Holds is loto-njgz's benchmark clause: with
// 200 holds in the store, an Edit pre-hook's admission decides in under 10ms.
// Decide reads only the rows on the edited path (store.LocksAt), so the cost
// does not grow with the hold count. The Edit lands on a peer's hold, so every
// call decides and none takes a lock. Same perf-tag and batch rules as above.
func TestHook_EditDecidesUnder10msAt200Holds(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("wall-clock bound; -race and -short runs do not measure the hook")
	}
	repo := withTempProject(t)
	pinAgent(t)
	const holds = 200
	lock := make([]string, 0, 1+holds+2)
	lock = append(lock, tcCmdLock)
	for i := range holds {
		name := fmt.Sprintf("held-%03d.txt", i)
		if err := os.WriteFile(filepath.Join(repo, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		lock = append(lock, name)
	}
	lock = append(lock, tcFlagIntent, tcIntentTest)
	if code := Run(lock, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("seed holds: exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t) // a second actor, editing the first one's hold
	rt, done := hookStoreRead(t)
	defer done()
	target := filepath.Join(repo, "held-100.txt")

	const batches, calls = 3, 51
	var medians []time.Duration
	for range batches {
		took := make([]time.Duration, 0, calls)
		for range calls {
			start := time.Now()
			if _, blocker := hookAdmit(t.Context(), rt, target, ""); blocker == nil {
				t.Fatal("the peer's hold did not refuse the Edit")
			}
			took = append(took, time.Since(start))
		}
		slices.Sort(took)
		medians = append(medians, took[calls/2])
		if took[calls/2] < 10*time.Millisecond {
			return
		}
	}
	t.Errorf("Edit admission p50 per batch = %v, want one < 10ms", medians)
}
