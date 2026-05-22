// Package identity manages per-session agent identities at ~/.loto/agents/<uuid>.json
// and resolves LOTO_AGENT_ID for the running process.
//
// Env contract:
//   - LOTO_AGENT_ID=<uuid>  : reuse that persisted identity
//   - LOTO_AGENT_ID=""      : mint an ephemeral in-memory identity (fleet mode)
//   - LOTO_AGENT_ID unset   : session cache → mostRecentAgent → newAgent
//   - LOTO_HANDLE=<name>    : preassign handle (PascalCase adjective+noun)
//   - CLAUDE_CODE_SESSION_ID=<sid> : when LOTO_AGENT_ID is unset, bind this
//     session to a single agent via ~/.loto/session/<sid>.json so concurrent
//     first-use callers in the same Claude Code session converge on one uuid.
//
// Ensure also runs a once-per-process GC pass over ~/.loto/agents/, deleting
// entries older than 30 days.
package identity
