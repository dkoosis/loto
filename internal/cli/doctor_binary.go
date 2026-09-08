package cli

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"strconv"
	"strings"
)

// unknownBuildField is what an unstamped build reports for a field it has no
// value for. Named rather than repeated so the fallback reads the same on
// `loto version` and on doctor's identity line, which must never disagree.
const unknownBuildField = "unknown"

// buildIdentity is the VCS stamp the Go toolchain records into the binary at
// build time. Empty fields mean the build carried no stamp — a `go build` with
// -buildvcs=false, or a test binary, which is neither an error nor a finding.
type buildIdentity struct {
	rev   string // full commit SHA
	built string // RFC3339 build time
	dirty bool   // built from a tree with uncommitted changes
}

// readBuildIdentity reads the running binary's stamp. Shared with `loto
// version` so the two surfaces cannot disagree about what this binary is.
func readBuildIdentity() buildIdentity {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return buildIdentity{}
	}
	var id buildIdentity
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			id.rev = s.Value
		case "vcs.time":
			id.built = s.Value
		case "vcs.modified":
			id.dirty = s.Value == "true"
		}
	}
	return id
}

// display names the identity's unstamped fields instead of leaving them blank:
// a blank field reads as a broken reader, "unknown" reads as the answer.
func (b buildIdentity) display() (rev, built string) {
	rev, built = b.rev, b.built
	if rev == "" {
		rev = unknownBuildField
	}
	if built == "" {
		built = unknownBuildField
	}
	return rev, built
}

// binaryStaleness is the comparison between the running binary's commit and
// the repo doctor is being run inside. The zero value means "no finding",
// which covers every case the comparison cannot be made in — an unstamped
// binary, a non-repo cwd, or a repo that is not loto's own source.
type binaryStaleness struct {
	stale    bool
	repoHead string
	behind   int
}

// checkBinaryStaleness reports whether the running binary predates repoTop's
// HEAD. Local git only: no network, and nothing read outside repoTop and the
// binary's own stamp.
//
// The `cat-file -e` probe is what keeps this honest in a repo that is not
// loto: doctor runs from any project, and a rev that is not an object here
// means there is nothing to compare rather than something to report. Strict
// ancestry is the test for "older" — a binary built from a side branch, or
// from work newer than HEAD, is a different situation and not this row's
// business.
func checkBinaryStaleness(ctx context.Context, repoTop string, id buildIdentity) binaryStaleness {
	if id.rev == "" || repoTop == "" {
		return binaryStaleness{}
	}
	if _, err := gitCmd(ctx, repoTop, "cat-file", "-e", id.rev+"^{commit}"); err != nil {
		return binaryStaleness{}
	}
	rawHead, err := gitCmd(ctx, repoTop, "rev-parse", "HEAD")
	if err != nil {
		return binaryStaleness{}
	}
	head := strings.TrimSpace(rawHead)
	if head == "" || head == id.rev {
		return binaryStaleness{}
	}
	if _, err := gitCmd(ctx, repoTop, "merge-base", "--is-ancestor", id.rev, head); err != nil {
		return binaryStaleness{}
	}
	behind := 0
	if out, cerr := gitCmd(ctx, repoTop, "rev-list", "--count", id.rev+".."+head); cerr == nil {
		behind, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	return binaryStaleness{stale: true, repoHead: head, behind: behind}
}

// renderBinaryIdentity writes the identity line every doctor run carries, and
// the ✗ row when the binary is behind the repo. The row never changes doctor's
// exit code: a stale binary is a report, not a broken box, and blocking a verb
// on it would take the machine down over its own diagnosis.
func renderBinaryIdentity(stdout io.Writer, repoTop string, id buildIdentity, st binaryStaleness) {
	rev, built := id.display()
	dirty := ""
	if id.dirty {
		dirty = " dirty=true"
	}
	fmt.Fprintf(stdout, "binary:  rev=%s built=%s%s\n", rev, built, dirty)
	if !st.stale {
		return
	}
	fmt.Fprintf(stdout, "✗ binary_stale binary=%s repo=%s behind=%d\n", id.rev, st.repoHead, st.behind)
	fmt.Fprintln(stdout, "```bash")
	fmt.Fprintf(stdout, "cd %s && make install\n", repoTop)
	fmt.Fprintln(stdout, "```")
}
