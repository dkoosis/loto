package cli

import (
	"strings"
	"testing"
)

// TestTagHelpTeachesContract pins the tag -h teaching surface (loto-5rwc):
// usage line, at least one example, and the bead-id tag-content convention.
func TestTagHelpTeachesContract(t *testing.T) {
	_, stderr, _ := executeCommand(tcCmdTag, "-h")
	for _, want := range []string{
		"usage: loto tag <file> <text...>",
		"loto-c6rg: want next", // example demonstrating <bead-id>: <=3-word ask
		"bead id",              // convention: open with requester's bead id
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("tag -h missing %q; got:\n%s", want, stderr)
		}
	}
	// Convention guards against drift: the worked examples must not put
	// epic/gh-issue into the tag content (the bead id resolves them).
	for line := range strings.SplitSeq(stderr, "\n") {
		if !strings.Contains(line, "loto tag ") {
			continue
		}
		if strings.Contains(line, "epic") || strings.Contains(line, "gh-issue") {
			t.Fatalf("tag -h example must not put epic/gh-issue in tag text: %q", line)
		}
	}
}

// TestLockHelpTeachesContract pins the lock -h teaching surface (loto-5rwc):
// usage line plus at least one worked example.
func TestLockHelpTeachesContract(t *testing.T) {
	_, stderr, _ := executeCommand(tcCmdLock, "-h")
	for _, want := range []string{
		`loto lock <target>`,
		`-t "`, // example shows the required intent flag
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("lock -h missing %q; got:\n%s", want, stderr)
		}
	}
}
