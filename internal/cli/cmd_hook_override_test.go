package cli

import (
	"os"
	"path/filepath"
	"testing"

	"loto/internal/store"
)

// TestHookOverride_RecordsEventNamingGuard is the bead's first and second
// acceptance criteria (loto-mh07), at the CLI layer rather than through a
// real git hook: `loto hook override <guard>` leaves one guard_override
// event naming the guard, for each of the three guards a bypass can name.
func TestHookOverride_RecordsEventNamingGuard(t *testing.T) {
	guards := []string{
		hookOverrideGuardPreCommit,
		hookOverrideGuardPostCheckout,
		hookOverrideGuardReferenceTransaction,
	}
	for _, guard := range guards {
		t.Run(guard, func(t *testing.T) {
			withTempProject(t)
			a := pinAgent(t)

			if _, stderr, code := executeCommand("hook", "override", guard); code != 0 {
				t.Fatalf("hook override %s: exit %d, stderr=%q", guard, code, stderr)
			}

			ev := latestRefEvent(t, store.EventGuardOverride)
			if ev.Reason != guard {
				t.Errorf("event reason = %q, want %q (the guard bypassed)", ev.Reason, guard)
			}
			if ev.ActorUUID != a.UUID {
				t.Errorf("event actor = %q, want %q", ev.ActorUUID, a.UUID)
			}
			if ev.Target.Canonical != "" {
				t.Errorf("an override is session-scoped, want no target, got %q", ev.Target.Canonical)
			}
		})
	}
}

func TestHookOverride_UnknownGuardIsUsageError(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	if _, _, code := executeCommand("hook", "override", "some-other-guard"); code != 2 {
		t.Errorf("unknown guard: exit %d, want 2", code)
	}
}

func TestHookOverride_WrongArgCountIsUsageError(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	if _, _, code := executeCommand("hook", "override"); code != 2 {
		t.Errorf("no guard: exit %d, want 2", code)
	}
	if _, _, code := executeCommand("hook", "override", hookOverrideGuardPreCommit, "extra"); code != 2 {
		t.Errorf("extra arg: exit %d, want 2", code)
	}
}

// TestHookOverride_StoreUnwritable_StillExits0 is the bead's third
// acceptance criterion. loto.db planted as a DIRECTORY rather than a file
// reproduces "the store cannot be written" without a permission trick a
// root-run CI could sidestep: store.OpenContext fails the same way a
// disk-full or corrupt-file open would, before any event is written.
func TestHookOverride_StoreUnwritable_StillExits0(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	base := os.Getenv("LOTO_BASE")
	if base == "" {
		t.Fatal("fixture: LOTO_BASE must be set by withTempProject")
	}
	if err := os.MkdirAll(filepath.Join(base, "loto.db"), 0o755); err != nil {
		t.Fatalf("plant unwritable store: %v", err)
	}

	_, stderr, code := executeCommand("hook", "override", hookOverrideGuardPreCommit)
	if code != 0 {
		t.Fatalf("store unwritable: exit %d, want 0; stderr=%q", code, stderr)
	}
	if stderr == "" {
		t.Error("a failed record should still say so on stderr")
	}
}
