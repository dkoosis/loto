// Package identity resolves the agent UUID that owns locks, claims and tags
// for the running process, and records enough about the session behind it
// to answer "is that holder still alive?" from a shell.
//
// There is no registry. Claude Code names sessions, lists peers and carries
// messages between them natively (ListAgents / SendMessage, CC ≥ 2.1.224),
// so loto no longer mints handles, keeps peer records, or answers `who` and
// `alive` (loto-jnid). What remains is the authority half: one stable owner
// id per session, derived from the environment Claude Code already exports.
//
// Governing principle: identity ambiguity is allowed for display, never for
// authority. Anything that acquires, releases, or attributes a lock runs
// under a stable, explicit, validated owner id or is refused.
//
// Resolution order (Ensure):
//
//  1. LOTO_AGENT_ID=<uuid>: an explicit pin, shape-validated. Nothing on disk
//     is consulted; the value is the owner.
//  2. LOTO_AGENT_ID="" (explicit empty): an ephemeral in-memory owner. Fleet
//     dispatchers export this for throwaway processes.
//  3. CLAUDE_CODE_SESSION_ID=<sid>: the owner is the actor, the Claude Code
//     process behind the session: a UUID derived from (host, pid, process
//     start time), read from env or the session record (actorOwner). Its
//     subagents and every session id /clear gives it are the same owner, so
//     nothing tells a subagent's write from its parent's (docs/write-gate.md).
//     With no process known the owner is the session id.
//  4. Nothing set: Ensure returns ErrUnpinned. Authority-bearing verbs refuse;
//     read-only verbs substitute Ephemeral() for display. Non-Claude-Code
//     callers (codex, `claude -p`, a raw shell) are not supported yet.
//
// Liveness: `loto whoami` (run by the SessionStart hook) records the session
// process — pid, start-time, messaging socket — at ~/.loto/session/<sid>.json
// (LOTO_BASE/session when LOTO_BASE is set, sd-kx5). ProbeSession reads that
// record to answer for a holder whose stamped lock pid is gone but whose
// session is still up (loto-r11w); a lock row's own pid/proc_start remains
// the fallback (internal/cli/runtime.go pidVerdict). A Go binary cannot call
// ListAgents, so this file is the one presence fact loto still keeps.
package identity
