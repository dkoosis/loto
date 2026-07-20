package domain

import (
	"errors"
	"strings"
	"time"
)

// MsgBodyMaxBytes caps a message body, mirroring the tag text cap: the mailbox
// carries asks and pointers, not documents.
const MsgBodyMaxBytes = 4096

// AddrAll is the broadcast address: every agent's inbox surfaces the message.
const AddrAll = "@all"

// Message is one store-and-forward mail item. Unlike a Tag it is addressed to
// an agent or a repo, not parasitic on a lock, and it lives in the host-global
// mail store so it crosses project boundaries. Read state is NOT a field here:
// broadcast and repo addresses are read independently by many agents, so read
// cursors live in a per-reader table keyed by (message id, reader uuid).
type Message struct {
	ID         string
	FromUUID   AgentUUID
	FromHandle string // denormalized at send; survives sender-record GC
	To         string // AddrAll, "@<repo-slug>", or an agent UUID
	Body       string
	ThreadID   string // optional; groups replies, "" = unthreaded
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

var (
	ErrMsgBodyEmpty    = errors.New("message body empty")
	ErrMsgBodyTooLarge = errors.New("message body exceeds 4096 bytes")
	ErrMsgBadAddr      = errors.New("message address empty")
)

// ValidateMessage enforces the send-side contract. Address resolution (handle →
// UUID) happens at the cli boundary; by the time a Message reaches the mail
// store To is a UUID or an @-address.
func ValidateMessage(m Message) error {
	switch {
	case strings.TrimSpace(m.Body) == "":
		return ErrMsgBodyEmpty
	case len(m.Body) > MsgBodyMaxBytes:
		return ErrMsgBodyTooLarge
	case m.To == "":
		return ErrMsgBadAddr
	}
	return nil
}

// RepoAddr returns the repo address for a project slug ("@<slug>"). Mail sent
// here is surfaced to whichever agent next acts inside that project — the
// "ask the agents over in repo X" channel when no specific agent is known.
//
// The literal slug "all" is reserved: "@all" is the machine-wide broadcast, so
// a repo whose computed slug is "all" would make its repo channel indistinct
// from broadcast (Codex). Such a slug folds to a namespaced address so @<slug>
// routing stays unambiguous — the pathological repo simply has no bare "@all"
// repo channel; "@all" always means broadcast.
func RepoAddr(slug string) string {
	if "@"+slug == AddrAll {
		return AddrAll + "-repo"
	}
	return "@" + slug
}

// IsBroadcastAddr reports whether addr fans out to more than one potential
// reader (@all or any @<slug>), i.e. read state must be tracked per reader.
func IsBroadcastAddr(addr string) bool { return strings.HasPrefix(addr, "@") }

// IsRepoAddr reports whether addr is a repo baton address (@<slug>): a note
// taped to a repo's door, consumed by the FIRST agent to read it rather than
// surfaced per-reader. @all (machine-wide broadcast) and bare-UUID direct mail
// are NOT baton addresses. Drives both the longer repo-mail TTL at send and the
// delete-on-first-read (baton) semantics at MarkRead.
func IsRepoAddr(addr string) bool {
	return strings.HasPrefix(addr, "@") && addr != AddrAll
}
