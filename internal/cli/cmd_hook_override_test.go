package cli

import (
	"os"
	"path/filepath"
	"strings"
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

// TestHookOverride_UnpinnedIdentitySkipsWithoutRecording is the coordinator
// review's second finding on loto-mh07: an unpinned caller — a human running
// git under the override, or any process with nothing in the environment
// naming an agent identity — must not write a guard_override row under a
// throwaway ephemeral UUID. That pollutes the exact signal this event kind
// exists to give a promotion. Skip, exit 0, say so on stderr.
func TestHookOverride_UnpinnedIdentitySkipsWithoutRecording(t *testing.T) {
	withTempProject(t)
	// No pinAgent(t) — withTempProject already unsets LOTO_AGENT_ID and
	// CLAUDE_CODE_SESSION_ID, so this call is unpinned.

	_, stderr, code := executeCommand("hook", "override", hookOverrideGuardPreCommit)
	if code != 0 {
		t.Fatalf("unpinned: exit %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "identity=unpinned") {
		t.Errorf("stderr should say why it skipped, got %q", stderr)
	}

	for _, ev := range readAllEvents(t) {
		if ev.Kind == store.EventGuardOverride {
			t.Fatalf("unpinned caller must not write a row, got %+v", ev)
		}
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
