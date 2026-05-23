// Package identity resolves a stable agent UUID for the running process and
// persists per-agent records at ~/.loto/agents/<uuid>.json. Ensure is the
// entry point; LookupByUUID and NewUUID are supporting surfaces.
//
// Governing principle: identity ambiguity is allowed for display, never for
// authority. Anything that acquires, releases, or attributes a lock must run
// under a stable, explicit, validated UUID binding. Heuristics are only
// consulted as a last-resort interactive convenience and are bounded.
//
// Resolution order (Ensure):
//
//  1. LOTO_AGENT_ID=<uuid>: shape-validated against the RFC 4122 v4 form
//     before any filesystem touch (prevents path escape). Must resolve to an
//     existing registry record; an unresolvable uuid is a hard error
//     (errStaleAgentID) — silently substituting an ephemeral identity would
//     orphan every lock acquired in this session.
//  2. LOTO_AGENT_ID="" (explicit empty): mint an ephemeral in-memory identity,
//     no disk record. Fleet dispatchers export this to keep the registry from
//     accumulating one orphan .json per subagent.
//  3. LOTO_AGENT_ID unset, CLAUDE_CODE_SESSION_ID=<sid> set: bind the session
//     to one agent via ~/.loto/session/<sid>.json. Concurrent first-use
//     callers race an O_EXCL claim; losers drop their candidate record and
//     adopt the winner's mapping (gh#28).
//  4. LOTO_AGENT_ID and CLAUDE_CODE_SESSION_ID both unset: mint a fresh
//     persistent agent. The prior heuristic that reused the most-recent
//     local agent within a 24h window was removed (gh#121): a dead session's
//     UUID could be adopted by a new process and silently re-attribute new
//     locks to it, violating the v4 "ambiguity allowed for display, never
//     for authority" invariant. Pin identity across processes by exporting
//     CLAUDE_CODE_SESSION_ID or LOTO_AGENT_ID.
//
// LOTO_HANDLE preassigns the agent handle; its shape is validated in
// handles.go but membership in the built-in word lists is not required.
//
// Ensure also runs a once-per-process GC pass over ~/.loto/agents/, deleting
// records older than 30 days — except any uuid still referenced by a session
// cache file. Preserving referenced uuids keeps the session→agent binding
// from being broken from underneath a live session. Session cache files are
// not themselves GC'd.
package identity
