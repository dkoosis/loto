package main

import (
	"errors"
	"fmt"
	"os"
)

var (
	errHookFromTerminal    = errors.New("loto hook: not for interactive use")
	errDashboardNoTerminal = errors.New("wrong-surface: dashboard TUI requires a terminal")
)

// envSurfaceTest forces the wrong-surface checks to a deterministic answer for
// tests. Values: "tty" → treat stdio as terminal; "pipe" → treat as non-tty.
// Empty/unset → real stat() of os.Stdin/os.Stdout.
const envSurfaceTest = "LOTO_SURFACE_TEST"

// stdinIsTerminal reports whether stdin is attached to a character device
// (a real terminal vs. a pipe/file). Used to detect humans invoking CC-only
// hook subcommands directly.
func stdinIsTerminal() bool {
	if v := os.Getenv(envSurfaceTest); v != "" {
		return v == "tty"
	}
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// stdoutIsTerminal reports whether stdout is attached to a character device.
// Used to detect CC (or any non-interactive caller) invoking the human-only
// dashboard TUI without `--format=llm`.
func stdoutIsTerminal() bool {
	if v := os.Getenv(envSurfaceTest); v != "" {
		return v == "tty"
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// denyHookFromTerminal prints a wrong-surface message to stderr and returns
// an error so cobra exits non-zero. Root command sets SilenceErrors=true, so
// we print explicitly here.
func denyHookFromTerminal(sub string) error {
	msg := fmt.Sprintf(`loto hook %s: not for interactive use
this command is invoked by Claude Code (reads tool-input JSON from stdin).
→ run `+"`loto install-hook --write-gate`"+` to wire it into Claude Code,
  or pipe JSON via stdin if you are testing manually
`, sub)
	fmt.Fprint(os.Stderr, msg)
	return fmt.Errorf("%w: %s", errHookFromTerminal, sub)
}

// denyDashboardWithoutTerminal denies the dashboard TUI when stdout is not a
// terminal. Output uses claude-optimized form per .claude/rules/design.md.
func denyDashboardWithoutTerminal() error {
	msg := `✗ wrong-surface: dashboard TUI requires a terminal
→ use ` + "`loto dashboard --format=llm`" + ` for the non-interactive event stream
`
	fmt.Fprint(os.Stderr, msg)
	return errDashboardNoTerminal
}
