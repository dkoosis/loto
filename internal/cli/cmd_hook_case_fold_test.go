package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"loto/internal/store"
)

// tcCaseFoldFile is the on-disk spelling git status reports; tcCaseFoldFold is
// the lowercase key foldTargetKey mints for either spelling. Neither is the
// all-lowercase tcTargetA, which has no case-variant spelling to confuse it
// with (loto-dwu9).
const (
	tcCaseFoldFile = "Roadmap.go"
	tcCaseFoldFold = "roadmap.go"
)

// TestHook_TreeEventFoldsObservedPathToTheLockKey is loto-dwu9: the pre-hook's
// observation set added git status's paths straight through, unfolded, while
// a peer's lock key went through foldTargetKey (loto-f8m8) — so on a
// case-folding filesystem one physical file could file under two
// path_canonical spellings, one read as peer-locked and the other as
// unattributed, for the same write.
//
// Alice locks the lowercase spelling; the file itself sits on disk spelled
// Roadmap.go, which is what `git status` prints for bob's dirtying write. The
// fix must fold that observed spelling to the same key alice's lock used, so
// exactly one event is filed, keyed like the lock, rule row5.
func TestHook_TreeEventFoldsObservedPathToTheLockKey(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	folds := caseVariantFSNote(t, repo)

	if err := os.WriteFile(filepath.Join(repo, tcCaseFoldFile), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !folds {
		// Two genuinely different files here — give the lock its own so the
		// setup below does not fail for want of something to lock.
		if err := os.WriteFile(filepath.Join(repo, tcCaseFoldFold), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcCaseFoldFold, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock on %q: exit %d", tcCaseFoldFold, code)
	}

	// Bob's call is a plain Bash tool use — no admission, so the observation
	// set is built entirely from live holders and `git status`, the two
	// inputs the bug let disagree on spelling.
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "bob-1", "", "")); code != 0 {
		t.Fatalf("bob pre: exit=%d err=%q", code, errOut)
	}
	if err := os.WriteFile(filepath.Join(repo, tcCaseFoldFile), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "bob-1", "", "")); code != 0 {
		t.Fatalf("bob post: exit=%d err=%q", code, errOut)
	}

	evsFolded := treeEvents(t, tcCaseFoldFold)
	evsRaw := treeEvents(t, tcCaseFoldFile)
	if !folds {
		if len(evsFolded) != 0 {
			t.Fatalf("case-sensitive filesystem: %q must be untouched, got %+v", tcCaseFoldFold, evsFolded)
		}
		return
	}
	if len(evsFolded) != 1 || len(evsRaw) != 0 {
		t.Fatalf("one physical file must file exactly one event keyed like the lock (%q), none under the raw observed spelling (%q); got %d at %q and %d at %q",
			tcCaseFoldFold, tcCaseFoldFile, len(evsFolded), tcCaseFoldFold, len(evsRaw), tcCaseFoldFile)
	}
	if evsFolded[0].Rule != store.TreeRuleRow5 {
		t.Errorf("holder_pre alice, observer bob, uncontested wants row5, got %s", evsFolded[0].Rule)
	}
	if string(evsFolded[0].HolderPre) != alice.UUID {
		t.Errorf("holder_pre must be alice, got %s", evsFolded[0].HolderPre)
	}
}
