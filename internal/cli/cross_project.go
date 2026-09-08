package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"loto/internal/render"
)

// beaconGroup is one project's share of a multi-target beacon call: the raw,
// caller-typed tokens that resolve into repoTop, plus whether repoTop IS the
// caller's own project (isCaller) or a foreign one this session must open a
// separate runtime for.
//
// isCaller matters beyond equality-of-string: the caller's own group must
// keep calling openRuntime(ctx) verbatim (which re-derives repoTop itself,
// including the errNotInGitRepo path for a caller standing outside any git
// repo) rather than openRuntimeForRepoTop(ctx, repoTop) — that is what keeps
// the single-project call shape byte-for-byte what it was before loto-72i
// (the golden-diff requirement in its AC).
type beaconGroup struct {
	repoTop  string
	rawArgs  []string
	isCaller bool
}

// groupTargetsByOwningProject partitions raw beacon targets by the git
// project that actually owns each one (loto-72i, Rule: "A lease or violation
// check on a path resolves to the project that OWNS the path, not the
// project the caller stands in").
//
// callerRepoTop=="" (the caller's cwd is not itself inside a git repo) is
// handled as a single wholesale fallback group so a repoless invocation keeps
// its exact pre-72i behavior — resolving per-target ownership needs at least
// one repo to compare against, and a caller with none has no "own project"
// for a same-project target to differ from anyway.
//
// invalid names, one row per path, every target that resolves into NO known
// git project at all (Rule: "refused... never silently leased against the
// caller's project").
func groupTargetsByOwningProject(ctx context.Context, args []string, base, callerRepoTop string) (groups []beaconGroup, invalid []render.InvalidTarget) {
	if callerRepoTop == "" {
		return []beaconGroup{{rawArgs: args, isCaller: true}}, nil
	}

	order := make([]string, 0, len(args))
	byTop := map[string][]string{}
	for _, raw := range args {
		top, ok := repoTopForPath(ctx, base, raw)
		if !ok {
			invalid = append(invalid, render.InvalidTarget{Path: raw, Reason: "no-owning-project"})
			continue
		}
		if _, seen := byTop[top]; !seen {
			order = append(order, top)
		}
		byTop[top] = append(byTop[top], raw)
	}
	if len(invalid) > 0 {
		return nil, invalid
	}

	groups = make([]beaconGroup, 0, len(order))
	for _, top := range order {
		groups = append(groups, beaconGroup{
			repoTop:  top,
			rawArgs:  byTop[top],
			isCaller: top == callerRepoTop,
		})
	}
	return groups, nil
}

// repoTopForPath resolves the git repository toplevel that owns path raw,
// independent of the calling process's own cwd — the piece single-project
// loto never needed: every target used to be judged against the one repoTop
// openRuntime itself resolves, so nothing asked "whose project is this path
// really in" as its own question.
//
// raw is joined onto base first when relative — base is the caller's cwd
// (callerBase()), the same provenance resolveCLITarget uses for a
// caller-typed relative token. Walking up to nearestExistingDir before
// invoking git matters because a beacon target may name a file that does not
// exist yet (loto-z5nb): the file has no answer to "what repo owns this",
// but its parent directory does.
//
// Returns ("", false) when raw resolves into no git repository at all, or
// when a relative raw has no usable base.
func repoTopForPath(ctx context.Context, base, raw string) (string, bool) {
	abs := raw
	if !filepath.IsAbs(abs) {
		if base == "" {
			return "", false
		}
		abs = filepath.Join(base, abs)
	}
	dir := nearestExistingDir(abs)
	if dir == "" {
		return "", false
	}
	out, err := gitCmd(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	top := strings.TrimSpace(out)
	if top == "" {
		return "", false
	}
	return top, true
}

// nearestExistingDir walks up from abs (an absolute path, existing or not) to
// the nearest ancestor directory that exists on disk. Returns "" only if
// every ancestor up to and including the filesystem root is missing, which
// does not happen for a real absolute path since the root always exists.
func nearestExistingDir(abs string) string {
	dir := filepath.Clean(abs)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
