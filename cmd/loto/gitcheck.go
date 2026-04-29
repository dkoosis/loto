package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"loto"
)

// ── check-paths ───────────────────────────────────────────────────────────────

var checkStagedFlag bool

var checkPathsCmd = &cobra.Command{
	Use:   "check-paths [path...]",
	Short: "check paths against active locks and reservations",
	Long: `Exits 1 if any path is held by another agent's exclusive lock or
matches another agent's advisory reservation. Designed for use as a git pre-commit hook.

Flags:
  --staged   read paths from 'git diff --name-only --cached'`,
	RunE: func(cmd *cobra.Command, args []string) error {
		paths := args
		if checkStagedFlag {
			staged, err := stagedPaths()
			if err != nil {
				exit(err)
			}
			paths = append(paths, staged...)
		}
		if len(paths) == 0 {
			return nil
		}

		l := newLOTO()
		myAgent := flagAgent

		type conflict struct {
			Path    string `json:"path"`
			Kind    string `json:"kind"`
			Holder  string `json:"holder"`
			Pattern string `json:"pattern,omitempty"`
			Intent  string `json:"intent,omitempty"`
		}
		var conflicts []conflict

		for _, p := range paths {
			tag, err := l.ReadTag(p)
			if err == nil && tag != nil && tag.AgentID != myAgent {
				conflicts = append(conflicts, conflict{
					Path:   p,
					Kind:   "lock",
					Holder: tag.AgentID,
					Intent: tag.Intent,
				})
			}

			res, err := l.ConflictingReservations(p)
			if err == nil {
				for _, r := range res {
					if r.AgentID != myAgent {
						conflicts = append(conflicts, conflict{
							Path:    p,
							Kind:    "reservation",
							Holder:  r.AgentID,
							Pattern: r.Pattern,
							Intent:  r.Intent,
						})
					}
				}
			}
		}

		if len(conflicts) == 0 {
			return nil
		}

		if flagJSON {
			printJSON(map[string]any{"conflicts": conflicts})
		} else {
			fmt.Fprintln(os.Stderr, "loto: commit blocked — staged paths conflict with active locks or reservations")
			for _, c := range conflicts {
				if c.Pattern != "" {
					fmt.Fprintf(os.Stderr, "  %s: %s matches reservation %q held by %s (%s)\n",
						c.Kind, c.Path, c.Pattern, c.Holder, c.Intent)
				} else {
					fmt.Fprintf(os.Stderr, "  %s: %s held by %s (%s)\n",
						c.Kind, c.Path, c.Holder, c.Intent)
				}
			}
		}
		os.Exit(1)
		return nil
	},
}

func init() {
	checkPathsCmd.Flags().BoolVar(&checkStagedFlag, "staged", false, "read paths from git diff --name-only --cached")
}

func stagedPaths() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "diff", "--name-only", "--cached").Output()
	if err != nil {
		return nil, &loto.ErrSystem{Op: "git diff --cached", Err: err}
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
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

const hookBeginMarker = "# loto:begin"
const hookEndMarker = "# loto:end"
const hookBlock = "# loto:begin\nloto check-paths --staged || exit 1\n# loto:end"

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
