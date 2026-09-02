package domain

import (
	"errors"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"./a", "a"},
		{"a//b", "a/b"},
		{tcStorePrefix, tcStorePrefix},
	}
	for _, c := range cases {
		got, err := Canonicalize(c.in)
		if err != nil {
			t.Fatalf("Canonicalize(%q) err: %v", c.in, err)
		}
		if got.Canonical != c.want {
			t.Errorf("Canonicalize(%q) = %+v; want canonical=%q", c.in, got, c.want)
		}
	}
}

func TestCanonicalizeRejectsRepoEscape(t *testing.T) {
	if _, err := Canonicalize("../../etc/passwd"); err == nil {
		t.Fatal("expected error for repo-escape target")
	}
}

func TestCanonicalizeRejectsAbsolutePath(t *testing.T) {
	for _, in := range []string{"/tmp/x", "/etc/passwd"} {
		if _, err := Canonicalize(in); err == nil {
			t.Errorf("expected error for absolute path %q", in)
		}
	}
}

func TestCanonicalizeRejectsBackslashPath(t *testing.T) {
	if _, err := Canonicalize(`a\b.go`); err == nil {
		t.Fatal("expected error for backslash in target (storage is POSIX-style)")
	}
}

func TestCanonicalizeRejectsNULTarget(t *testing.T) {
	if _, err := Canonicalize("a\x00b"); err == nil {
		t.Fatal("expected error for NUL byte in target")
	}
}

func TestCanonicalizeRejectsGlob(t *testing.T) {
	for _, in := range []string{"*.go", "a/b/*.go", "a/?.go", "a/[abc].go", "a/{x,y}.go"} {
		_, err := Canonicalize(in)
		if !errors.Is(err, ErrTargetIsGlob) {
			t.Errorf("Canonicalize(%q) err=%v; want ErrTargetIsGlob", in, err)
		}
	}
}

func TestCanonicalizePrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{tcStorePrefix, tcStorePrefix},
		{"internal/store/", tcStorePrefix},
		{"internal/store//", tcStorePrefix},
		{"./internal/store", tcStorePrefix},
		{"a//b", "a/b"},
	}
	for _, c := range cases {
		got, err := CanonicalizePrefix(c.in)
		if err != nil {
			t.Fatalf("CanonicalizePrefix(%q) err: %v", c.in, err)
		}
		if got.Canonical != c.want {
			t.Errorf("CanonicalizePrefix(%q) = %+v; want canonical=%q", c.in, got, c.want)
		}
	}
}

func TestCanonicalizePrefixRejections(t *testing.T) {
	cases := []struct {
		in      string
		wantErr error
	}{
		{"", ErrEmptyTarget},
		{"a/*/b", ErrTargetIsGlob},
		{"a\x00b", ErrTargetHasNUL},
		{`a\b`, ErrTargetBackslash},
		{"../escape", ErrRepoEscape},
		{"/abs/path", ErrRepoEscape},
		{".", ErrTargetIsRepoRoot},
		{"./", ErrTargetIsRepoRoot},
	}
	for _, c := range cases {
		_, err := CanonicalizePrefix(c.in)
		if !errors.Is(err, c.wantErr) {
			t.Errorf("CanonicalizePrefix(%q) err=%v; want %v", c.in, err, c.wantErr)
		}
	}
}

func TestCanonicalizeRejectsTrailingSlash(t *testing.T) {
	for _, in := range []string{"foo/", "a/b/"} {
		_, err := Canonicalize(in)
		if !errors.Is(err, ErrTargetIsDir) {
			t.Errorf("Canonicalize(%q) err=%v; want ErrTargetIsDir", in, err)
		}
	}
}

// TestCanonicalizeRejectsShellTokens pins loto-bl66: a token the shell would
// have rewritten is not a path, and every verb must refuse it at the same
// place. The live defect was a lock on the literal string "$FAKE_HOME",
// minted by the PreToolUse gate and unreconcilable by any scan — the path does
// not exist, so nothing releases it but TTL.
func TestCanonicalizeRejectsShellTokens(t *testing.T) {
	for _, in := range []string{
		"$FAKE_HOME",
		"$PROBE_VAR",
		"a/$VAR/b.go",
		"`whoami`",
		`"quoted.go"`,
		"'quoted.go'",
		" leading.go",
		"trailing.go ",
		"two\nlines.go",
		"tab\there.go",
	} {
		if _, err := Canonicalize(in); !errors.Is(err, ErrTargetUnspellable) {
			t.Errorf("Canonicalize(%q) err = %v; want ErrTargetUnspellable", in, err)
		}
	}
}

// TestCanonicalizeKeepsAwkwardButLegalNames is the other half of the same
// contract, and the reason the rule stops short of the PreToolUse hook's own
// character class: an interior space is awkward but legal on every filesystem
// loto runs on, and refusing it would regress `loto lock` on a file that
// really exists.
func TestCanonicalizeKeepsAwkwardButLegalNames(t *testing.T) {
	for _, in := range []string{
		"my file.go",
		"dir with space/x.go",
		"weird-but-fine!.go",
		"at@sign.go",
		"plus+name.go",
	} {
		if _, err := Canonicalize(in); err != nil {
			t.Errorf("Canonicalize(%q) err = %v; want it accepted", in, err)
		}
	}
}

// TestCanonicalizePrefixInheritsShellTokenRule proves claim's surface cannot
// drift from the file verbs': CanonicalizePrefix delegates, so one policy
// source governs both (loto-bl66 AC #3).
func TestCanonicalizePrefixInheritsShellTokenRule(t *testing.T) {
	if _, err := CanonicalizePrefix("$FAKE_HOME/"); !errors.Is(err, ErrTargetUnspellable) {
		t.Errorf("CanonicalizePrefix err = %v; want ErrTargetUnspellable", err)
	}
}
