package cli

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testRevOld = "1111111111111111111111111111111111111111"
	testRevNew = "2222222222222222222222222222222222222222"
)

// TestBinaryIdentity_StaleNamesBothSidesAndTheFix pins the case loto-jhbm was
// filed on: the row has to carry both identities, because a reader who sees
// only one cannot tell which side is behind.
func TestBinaryIdentity_StaleNamesBothSidesAndTheFix(t *testing.T) {
	var out bytes.Buffer
	id := buildIdentity{rev: testRevOld, built: "2026-09-05T21:46:58Z"}
	renderBinaryIdentity(&out, "/repo/loto", id, binaryStaleness{stale: true, repoHead: testRevNew, behind: 30})

	got := out.String()
	for _, want := range []string{
		"binary:  rev=" + testRevOld + " built=2026-09-05T21:46:58Z\n",
		"✗ binary_stale binary=" + testRevOld + " repo=" + testRevNew + " behind=30\n",
		"```bash\ncd '/repo/loto' && make install\n```\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stale report missing %q; got:\n%s", want, got)
		}
	}
}

// TestBinaryIdentity_CurrentReportsWithoutAFinding is the other half of the
// AC: a current binary still says what it is, and says nothing more.
func TestBinaryIdentity_CurrentReportsWithoutAFinding(t *testing.T) {
	var out bytes.Buffer
	id := buildIdentity{rev: testRevNew, built: "2026-09-08T15:58:22Z"}
	renderBinaryIdentity(&out, "/repo/loto", id, binaryStaleness{})

	got := out.String()
	if got != "binary:  rev="+testRevNew+" built=2026-09-08T15:58:22Z\n" {
		t.Errorf("a current binary must print its identity and nothing else; got:\n%s", got)
	}
}

// TestBinaryIdentity_UnstampedStillSpeaks — silence would read as a crash, and
// "unknown" is itself the answer a reader needs (design.md: explicit
// empty-status, never silence).
func TestBinaryIdentity_UnstampedStillSpeaks(t *testing.T) {
	var out bytes.Buffer
	renderBinaryIdentity(&out, "/repo/loto", buildIdentity{}, binaryStaleness{})

	if got := out.String(); got != "binary:  rev=unknown built=unknown\n" {
		t.Errorf("an unstamped build must still report itself; got: %q", got)
	}
}

// TestBinaryIdentity_DirtyIsReportedNotFlagged: a build carrying uncommitted
// work matches no commit honestly, so the identity line says so — but it is
// not "older than the repo" and must not produce the ✗ row.
func TestBinaryIdentity_DirtyIsReportedNotFlagged(t *testing.T) {
	var out bytes.Buffer
	id := buildIdentity{rev: testRevNew, built: "2026-09-08T15:58:22Z", dirty: true}
	renderBinaryIdentity(&out, "/repo/loto", id, binaryStaleness{})

	got := out.String()
	if !strings.Contains(got, "dirty=true") {
		t.Errorf("a dirty build must say so; got: %q", got)
	}
	if strings.Contains(got, "✗") {
		t.Errorf("dirty is not stale — no finding row expected; got: %q", got)
	}
}

// TestCheckBinaryStaleness_AncestryDecidesIt walks the real comparison against
// a real repo: an ancestor commit is stale, HEAD is not, and a rev this repo
// has never heard of is not comparable at all — the last being every run of
// doctor from a project that is not loto's own source.
func TestCheckBinaryStaleness_AncestryDecidesIt(t *testing.T) {
	repo := t.TempDir()
	initBareGitRepo(t, repo)
	first := commitEmpty(t, repo, "first")
	head := commitEmpty(t, repo, "second")

	ctx := context.Background()

	if got := checkBinaryStaleness(ctx, repo, buildIdentity{rev: first}); !got.stale {
		t.Errorf("a binary built at an ancestor commit is stale; got %+v", got)
	} else {
		if got.repoHead != head {
			t.Errorf("stale row must name the repo's HEAD: got %s want %s", got.repoHead, head)
		}
		if got.behind != 1 {
			t.Errorf("behind count: got %d want 1", got.behind)
		}
	}

	if got := checkBinaryStaleness(ctx, repo, buildIdentity{rev: head}); got.stale {
		t.Errorf("a binary built at HEAD is not stale; got %+v", got)
	}

	if got := checkBinaryStaleness(ctx, repo, buildIdentity{rev: testRevOld}); got.stale {
		t.Errorf("a rev this repo does not contain is not comparable, so not stale; got %+v", got)
	}

	if got := checkBinaryStaleness(ctx, repo, buildIdentity{}); got.stale {
		t.Errorf("an unstamped binary cannot be compared; got %+v", got)
	}

	// A dirty build's rev names its starting commit, not everything the
	// binary contains, so ancestry comparison has nothing honest to report —
	// even against a rev that is otherwise a clean ancestor (loto-jhbm review).
	if got := checkBinaryStaleness(ctx, repo, buildIdentity{rev: first, dirty: true}); got.stale {
		t.Errorf("a dirty build is not comparable by ancestry; got %+v", got)
	}
}

// commitEmpty adds one empty commit to dir and returns its full SHA.
func commitEmpty(t *testing.T, dir, msg string) string {
	t.Helper()
	if out, err := runGit(dir, "commit", "--allow-empty", "-q", "-m", msg); err != nil {
		t.Fatalf("git commit %q: %v\n%s", msg, err, out)
	}
	sha, err := runGit(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v\n%s", err, sha)
	}
	return strings.TrimSpace(sha)
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(dir, "no-such-gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
