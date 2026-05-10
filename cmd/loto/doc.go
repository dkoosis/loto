// Command loto coordinates concurrent file edits across Claude Code sessions
// (and the subagents and shells they spawn) working in the same repository.
//
// # Inspired by industrial Lock-Out / Tag-Out
//
// In high-voltage maintenance, a technician cuts power, then attaches a
// physical lock to the breaker so it cannot be re-energized while they
// work. Alongside the lock they hang a tag identifying themselves and the
// reason. The lock is the enforcement mechanism — without the right key,
// you cannot move the breaker. The tag is the explanation — who, why,
// when. Lock without tag is anonymous and dangerous; tag without lock is a
// post-it note on a live breaker.
//
// loto applies the same discipline to source files. When an agent claims
// a file:
//
//   - The lock prevents another writer from modifying the file. It must
//     be enforced at a layer the writer cannot ignore by accident — POSIX
//     file mode, kernel flock, or filesystem-level immutability.
//   - The tag records who holds it, the stated intent, the expiry, and
//     a mailbox for messages from blocked peers.
//
// Both halves are required. A tag without a lock is the post-it. A lock
// without a tag is anonymous obstruction — blocked peers see only the
// EWOULDBLOCK and have nothing to act on.
//
// # The protected colleague is your future self (and your other shells)
//
// Five Claude sessions in five worktrees of the same repo are not
// adversaries — but they will silently clobber each other's work if
// nothing prevents it. The threat model is "concurrent honest writers
// who do not know about each other," same as the electrician on the next
// shift. The lock is what lets them safely not know.
//
// # Design north star
//
// See docs/NORTH_STAR.md for the full design contract — four coordination
// tiers (reservation, record/TTL, file flock, global flock), invariants
// (flock is truth except for the bounded record-tier carve-out), and the
// five-Claude acceptance scenario. Every change to this command must be
// audited against that document; the doc is the contract, the code is
// just one implementation.
package main
