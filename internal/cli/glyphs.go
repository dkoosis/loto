package cli

// Output glyphs from .claude/rules/design.md. Centralized so the lint suite
// stops flagging every site as a duplicate string literal.
const (
	gOK    = "✓"
	gWarn  = "⚠"
	gFail  = "✗"
	gInfo  = "ℹ"
	gCheck = "✔"
)
