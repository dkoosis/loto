package main

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"loto"
	"loto/internal/render"
)

// ── check-paths ───────────────────────────────────────────────────────────────

// pathConflict captures one path-vs-holder conflict surfaced by check-paths.
type pathConflict struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"` // "lock" or "reservation"
	Holder  string `json:"holder"`
	Pattern string `json:"pattern,omitempty"`
	Intent  string `json:"intent,omitempty"`
}

func checkPathsCmd() *cobra.Command {
	var staged bool
	c := &cobra.Command{
		Use:   "check-paths [path...]",
		Short: "check paths against active locks and reservations",
		Long: `Exits 1 if any path is held by another agent's exclusive lock or
matches another agent's advisory reservation. Designed for use as a git pre-commit hook.

Flags:
  --staged   read paths from 'git diff --name-only --cached'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			paths := args
			if staged {
				stagedList, err := stagedPaths()
				if err != nil {
					exit(err)
				}
				paths = append(paths, stagedList...)
			}
			conflicts := collectPathConflicts(newLOTO(), flagAgent, paths)
			out := os.Stdout
			if len(conflicts) > 0 {
				out = os.Stderr
			}
			if currentFormat == render.FormatJSON {
				payload := conflicts
				if payload == nil {
					payload = []pathConflict{}
				}
				_ = render.EmitJSON(out, map[string]any{"conflicts": payload})
			} else {
				rows := toRenderConflicts(conflicts)
				sortCheckPathsConflicts(rows)
				_ = render.EmitLLMCheckPaths(out, rows)
			}
			if len(conflicts) > 0 {
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&staged, "staged", false, "read paths from git diff --name-only --cached")
	return c
}

// collectPathConflicts walks paths and returns every lock/reservation conflict
// held by an agent other than myAgent.
func collectPathConflicts(l *loto.LOTO, myAgent string, paths []string) []pathConflict {
	var conflicts []pathConflict
	for _, p := range paths {
		if tag, err := l.ReadTag(p); err == nil && tag != nil && tag.AgentID != myAgent {
			conflicts = append(conflicts, pathConflict{
				Path:   p,
				Kind:   "lock",
				Holder: tag.AgentID,
				Intent: tag.Intent,
			})
		}
		res, err := l.ConflictingReservations(p)
		if err != nil {
			continue
		}
		for _, r := range res {
			if r.AgentID == myAgent {
				continue
			}
			conflicts = append(conflicts, pathConflict{
				Path:    p,
				Kind:    "reservation",
				Holder:  r.AgentID,
				Pattern: r.Pattern,
				Intent:  r.Intent,
			})
		}
	}
	return conflicts
}

// toRenderConflicts adapts cmd-side pathConflict to the render struct.
func toRenderConflicts(in []pathConflict) []render.CheckPathsConflict {
	out := make([]render.CheckPathsConflict, len(in))
	for i, c := range in {
		out[i] = render.CheckPathsConflict{
			Kind:    c.Kind,
			Path:    c.Path,
			Holder:  c.Holder,
			Intent:  c.Intent,
			Pattern: c.Pattern,
		}
	}
	return out
}

// sortCheckPathsConflicts orders by (kind, path, pattern, holder) so the same
// input always produces byte-identical output.
func sortCheckPathsConflicts(rows []render.CheckPathsConflict) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Pattern != b.Pattern {
			return a.Pattern < b.Pattern
		}
		return a.Holder < b.Holder
	})
}

func stagedPaths() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "diff", "--name-only", "--cached").Output()
	if err != nil {
		return nil, &loto.ErrSystem{Op: "git diff --cached", Err: err}
	}
	var paths []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// ── install-git-hook ──────────────────────────────────────────────────────────

var installGitHookCmd = &cobra.Command{
	Use:   "install-git-hook",
	Short: "write .git/hooks/pre-commit to enforce loto checks on staged files",
	Long: `Writes (or updates) .git/hooks/pre-commit to call 'loto check-paths --staged'.
The hook is idempotent: re-running replaces only the loto section.
If a pre-commit hook already exists its content is preserved; the loto
block is appended (or updated in place if already present).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := writeGitPreCommitHook(); err != nil {
			exit(err)
		}
		printJSON(map[string]any{"installed": true, "file": ".git/hooks/pre-commit"})
		return nil
	},
}

const (
	hookBeginMarker = "# loto:begin"
	hookEndMarker   = "# loto:end"
	hookBlock       = "# loto:begin\nloto check-paths --staged || exit 1\n# loto:end"
)

func writeGitPreCommitHook() error {
	hookPath := ".git/hooks/pre-commit"
	if err := os.MkdirAll(".git/hooks", 0o755); err != nil {
		return &loto.ErrSystem{Op: "create .git/hooks", Err: err}
	}

	existing, err := os.ReadFile(hookPath)
	if err != nil && !os.IsNotExist(err) {
		return &loto.ErrSystem{Op: "read pre-commit hook", Err: err}
	}

	var content string
	if len(existing) == 0 {
		content = "#!/bin/sh\n" + hookBlock + "\n"
	} else {
		s := string(existing)
		begin := strings.Index(s, hookBeginMarker)
		end := strings.Index(s, hookEndMarker)
		if begin >= 0 && end > begin {
			// Replace existing loto block in place.
			content = s[:begin] + hookBlock + s[end+len(hookEndMarker):]
		} else {
			// Append to existing hook.
			if !strings.HasSuffix(s, "\n") {
				s += "\n"
			}
			content = s + "\n" + hookBlock + "\n"
		}
	}

	if err := os.WriteFile(hookPath, []byte(content), 0o755); err != nil { //nolint:gosec // G306: git hooks must be executable
		return &loto.ErrSystem{Op: "write pre-commit hook", Err: err}
	}
	return nil
}
