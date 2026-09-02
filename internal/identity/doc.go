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
//  1. LOTO_SUBAGENT_ID=<stamp> with a session id present: the owner is a UUID
//     derived from (session id, stamp), so /team siblings that inherit one
//     session diverge into distinct owners (loto-fs84). A stamp with no
//     session id, or a malformed stamp, is ignored — fail-open by contract,
//     the stamp is never load-bearing (see resolveSubagent).
//  2. LOTO_AGENT_ID=<uuid>: an explicit pin, shape-validated. Nothing on disk
//     is consulted; the value is the owner.
//  3. LOTO_AGENT_ID="" (explicit empty): an ephemeral in-memory owner. Fleet
//     dispatchers export this for throwaway processes.
//  4. CLAUDE_CODE_SESSION_ID=<sid>: the owner IS the session id. Every
//     shell-out from one Claude Code session shares it, so every lock a
//     session takes is owned by one id with no cache to keep in sync.
//  5. Nothing set: Ensure returns ErrUnpinned. Authority-bearing verbs refuse;
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
