package cli

import (
	"strings"
	"testing"
)

// FuzzParseWorktreePorcelain feeds arbitrary text to the reader of
// `git worktree list --porcelain`. It must never panic, and every record it
// returns must carry a path.
func FuzzParseWorktreePorcelain(f *testing.F) {
	f.Add("worktree /a\nHEAD abc\nbranch refs/heads/main\n\nworktree /b\nbare\n")
	f.Add("worktree\n\n\nlocked reason\n")
	f.Add("HEAD abc\n")
	f.Fuzz(func(t *testing.T, out string) {
		for _, r := range parseWorktreePorcelain(out) {
			if r.Path == "" {
				t.Fatalf("record without a path from %q", out)
			}
		}
	})
}

// FuzzParseRefTransaction feeds arbitrary text to the reference-transaction
// hook's stdin reader. It must never panic, and every update it returns must
// name a ref.
func FuzzParseRefTransaction(f *testing.F) {
	f.Add("0000 abcd refs/heads/main\n")
	f.Add("ref:refs/heads/x abcd HEAD\r\n")
	f.Add("only-one-field\n\n  \n")
	f.Fuzz(func(t *testing.T, in string) {
		for _, u := range parseRefTransaction(strings.NewReader(in)) {
			if u.Ref == "" {
				t.Fatalf("update without a ref from %q", in)
			}
		}
	})
}
