package cli

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// stashAgentUnknown and stashPathsNone are named so the two literals below
// don't add a third goconst-tripping occurrence of strings the rest of the
// package already repeats (cmd_version.go's "unknown", cmd_lane.go/cmd_sync.go's
// "none").
const (
	stashAgentUnknown = "unknown"
	stashPathsNone    = "none"
)

// stashAgentStamp matches a `LOTO_AGENT_ID=<id>` token a fleet rescue-stash
// message may carry, anywhere in the message text. Stamping the token into
// rescue-stash messages is a consumer-repo bead (cc-plugins), not this one —
// this side only reads the stamp if present (loto-kotb Givens).
var stashAgentStamp = regexp.MustCompile(`LOTO_AGENT_ID=(\S+)`)

// danglingStash is one `git stash list` entry doctor considers worth a row.
type danglingStash struct {
	Ref       string
	CreatedAt time.Time
	Agent     string // "" when unattributable; renderer prints stashAgentUnknown
	Paths     []string
}

// scanDanglingStashes lists every stash in repoTop and keeps the ones whose
// creating agent is not live: an unattributable stash (no LOTO_AGENT_ID stamp)
// always counts as not live — there is no agent to check liveness against —
// and a stamped one is live exactly when that agent currently holds a lock
// (liveAgents membership, the same set runtime.go's lockOwnerUUIDs feeds to
// GCSessions — this doctor pass has no other notion of "an agent is doing
// something" to reuse without inventing a second one).
//
// Report only — this never pops, applies, or drops a stash (D8, "no silent
// dispossession of bytes", nug b2b0a9df507c; loto-m2nr owns guarding `git
// stash` itself, not this bead). A git failure (no stashes, no git, not a
// repo) is swallowed to nil rather than surfaced as a doctor error — the
// stash report is advisory, not load-bearing for doctor's own exit code.
//
// `git stash list` itself orders entries most-recent-first, and that is the
// order this returns — deterministic for a given repo state with no extra
// sort needed.
func scanDanglingStashes(ctx context.Context, repoTop string, liveAgents map[string]struct{}) []danglingStash {
	if repoTop == "" {
		return nil
	}
	raw, err := gitCmd(ctx, repoTop, "stash", "list", "--format=%gd%x1f%ct%x1f%s")
	if err != nil {
		return nil
	}
	var out []danglingStash
	for line := range strings.SplitSeq(strings.TrimRight(raw, "\n"), "\n") {
		s, ok := parseStashLine(line)
		if !ok {
			continue
		}
		if _, live := liveAgents[s.Agent]; s.Agent != "" && live {
			continue
		}
		s.Paths = stashPaths(ctx, repoTop, s.Ref)
		out = append(out, s)
	}
	return out
}

// parseStashLine decodes one `--format=%gd%x1f%ct%x1f%s` line into a
// danglingStash (Paths unset — the caller fills that in separately, since it
// costs its own git invocation). false on a blank or malformed line, e.g. an
// unexpected git version's stash-list format.
func parseStashLine(line string) (danglingStash, bool) {
	if line == "" {
		return danglingStash{}, false
	}
	parts := strings.SplitN(line, "\x1f", 3)
	if len(parts) != 3 {
		return danglingStash{}, false
	}
	sec, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return danglingStash{}, false
	}
	agent := ""
	if m := stashAgentStamp.FindStringSubmatch(parts[2]); m != nil {
		agent = m[1]
	}
	return danglingStash{Ref: parts[0], CreatedAt: time.Unix(sec, 0), Agent: agent}, true
}

// stashPaths lists the files one stash entry touches, via `git stash show
// --name-only`. Swallowed to nil on error: the ref/age/agent row still
// prints without it — paths is the least essential field the Rules ask for.
func stashPaths(ctx context.Context, repoTop, ref string) []string {
	raw, err := gitCmd(ctx, repoTop, "stash", "show", "--name-only", ref)
	if err != nil {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(strings.TrimRight(raw, "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// renderDanglingStashes prints one ℹ row per stash — advisory, never a
// pass/fail verdict (design.md: ℹ is neutral data, neither pass nor fail) —
// plus a count header when non-empty. Silent at zero so a repo with no
// dangling stashes keeps doctor's existing byte-identical output.
func renderDanglingStashes(w io.Writer, now time.Time, stashes []danglingStash) {
	if len(stashes) == 0 {
		return
	}
	fmt.Fprintf(w, "ℹ dangling_stashes count=%d\n", len(stashes))
	for _, s := range stashes {
		agent := s.Agent
		if agent == "" {
			agent = stashAgentUnknown
		}
		paths := stashPathsNone
		if len(s.Paths) > 0 {
			paths = strings.Join(s.Paths, ",")
		}
		fmt.Fprintf(w, "ℹ stash ref=%s age=%s agent=%s paths=%s\n",
			s.Ref, now.Sub(s.CreatedAt).Round(time.Second), agent, paths)
	}
}
