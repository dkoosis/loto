package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
)

// agentIDShape matches the canonical UUID hex layout. It is not a strict
// RFC 4122 v4 check — version/variant bits aren't enforced — because its
// job is to block path traversal and garbage before a value becomes an
// owner id, not to police provenance. newUUID and deriveUUID always emit
// v4-shaped values, so anything this package mints satisfies both.
var agentIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewUUID returns a fresh RFC 4122 v4 UUID. Exported so non-identity callers
// (e.g. CLI runtime session-id minting) can use the same generator without
// duplicating the bit-twiddling.
func NewUUID() string { return newUUID() }

func newUUID() string {
	var b [16]byte
	// crypto/rand.Read on Linux/macOS is backed by getrandom(2) / arc4random;
	// a failure here means the kernel CSPRNG is unavailable, which is a
	// program-environment failure, not a user error. Panic rather than
	// emit a zeroed (and thus colliding) "uuid".
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("identity: crypto/rand unavailable: %w", err))
	}
	return formatUUID(b)
}

// deriveUUID maps (session id, subagent stamp) to a stable v4-shaped UUID:
// the first 16 bytes of SHA-256 over "sid/stamp" with the version and
// variant bits set. Deterministic, so a sibling re-derives the same owner on
// every invocation with no cache file to race for (loto-jnid); distinct per
// stamp, so siblings sharing one session never collapse onto one owner
// (loto-fs84).
func deriveUUID(sid, stamp string) string {
	sum := sha256.Sum256([]byte(sid + "/" + stamp))
	var b [16]byte
	copy(b[:], sum[:16])
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

// sessionIDShape is the charset a CLAUDE_CODE_SESSION_ID must satisfy before
// it may become an owner id. Deliberately looser than agentIDShape — Claude
// Code emits UUIDs, but a test harness or an override may pin any opaque
// token — and still tight enough that the value can be spliced into the
// places an owner id reaches without changing their meaning: a git
// author/committer email (cmd_lane.go laneIdentity; `git commit-tree` rejects
// an ident containing a space, '<', '>' or a newline), a one-row-per-line
// render surface the PreToolUse hook parses, and a session record filename.
var sessionIDShape = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
