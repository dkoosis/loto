// Package mail is the host-global store-and-forward mailbox: one SQLite DB per
// machine (not per project), addressed by agent UUID, @<repo-slug>, or @all.
// It is deliberately a separate store from the per-project lock DB — mail must
// cross project boundaries, and coupling it to the lock schema is what killed
// the gen-2 mailbox (loto-vra). Read state is per reader (message_reads), so
// broadcast and repo addresses deliver to every agent independently.
//
// Everything here is coordination hygiene, not invariant: callers on hot paths
// (footers, banners) treat errors as "no mail" and stay silent.
package mail

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // database/sql driver

	"loto/internal/domain"
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
  id          TEXT PRIMARY KEY,
  from_uuid   TEXT NOT NULL,
  from_handle TEXT NOT NULL DEFAULT '',
  to_addr     TEXT NOT NULL,
  body        TEXT NOT NULL CHECK (length(body) <= 4096),
  thread_id   TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_to ON messages(to_addr, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_expires ON messages(expires_at);

CREATE TABLE IF NOT EXISTS message_reads (
  message_id  TEXT NOT NULL,
  reader_uuid TEXT NOT NULL,
  read_at     INTEGER NOT NULL,
  PRIMARY KEY (message_id, reader_uuid)
);
`

// DefaultTTL bounds unanswered mail. A week outlives any session burst while
// keeping the global DB self-cleaning; senders override per message via --ttl.
const DefaultTTL = 7 * 24 * time.Hour

// Box is an open handle on the host-global mail DB.
type Box struct {
	db *sql.DB
}

// Open opens (creating if needed) the mail DB at path and purges expired
// messages. Unlike the project store there is no corruption-recovery or
// flock ceremony: SQLite WAL + busy_timeout is enough for an append-mostly
// mailbox, and losing mail to a corrupt DB is acceptable — it is a channel,
// not a system of record.
func Open(ctx context.Context, path string) (*Box, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("mail schema: %w", err)
	}
	b := &Box{db: db}
	b.purgeExpired(ctx)
	return b, nil
}

func (b *Box) Close() error { return b.db.Close() }

// purgeExpired hard-deletes expired messages and their read rows. Best-effort:
// expiry is also enforced by every read query, so a failed purge only delays
// space reclamation.
func (b *Box) purgeExpired(ctx context.Context) {
	now := time.Now().UnixNano()
	_, _ = b.db.ExecContext(ctx,
		`DELETE FROM message_reads WHERE message_id IN (SELECT id FROM messages WHERE expires_at <= ?)`, now)
	_, _ = b.db.ExecContext(ctx, `DELETE FROM messages WHERE expires_at <= ?`, now)
}

// Send stores m. A zero ExpiresAt gets DefaultTTL from CreatedAt; a zero ID is
// minted (8 random bytes, "m-" prefix, mirroring the store's event IDs).
func (b *Box) Send(ctx context.Context, m domain.Message) (string, error) {
	if err := domain.ValidateMessage(m); err != nil {
		return "", err
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	if m.ExpiresAt.IsZero() {
		m.ExpiresAt = m.CreatedAt.Add(DefaultTTL)
	}
	if m.ID == "" {
		m.ID = newMsgID()
	}
	_, err := b.db.ExecContext(ctx,
		`INSERT INTO messages(id,from_uuid,from_handle,to_addr,body,thread_id,created_at,expires_at) VALUES (?,?,?,?,?,?,?,?)`,
		m.ID, string(m.FromUUID), m.FromHandle, m.To, m.Body, m.ThreadID,
		m.CreatedAt.UnixNano(), m.ExpiresAt.UnixNano())
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

// Inbox lists live messages addressed to any of addrs that reader has not yet
// read, oldest first (created_at then id — deterministic on ties). addrs is
// the reader's full address set: their UUID, @all, and the @<slug> of the repo
// they are acting in.
func (b *Box) Inbox(ctx context.Context, reader domain.AgentUUID, addrs []string) ([]domain.Message, error) {
	if len(addrs) == 0 {
		return nil, nil
	}
	q, args := inboxQuery(reader, addrs, time.Now())
	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Message
	for rows.Next() {
		var m domain.Message
		var from string
		var createdNs, expiresNs int64
		if err := rows.Scan(&m.ID, &from, &m.FromHandle, &m.To, &m.Body, &m.ThreadID, &createdNs, &expiresNs); err != nil {
			return nil, err
		}
		m.FromUUID = domain.AgentUUID(from)
		m.CreatedAt = time.Unix(0, createdNs)
		m.ExpiresAt = time.Unix(0, expiresNs)
		out = append(out, m)
	}
	return out, rows.Err()
}

func inboxQuery(reader domain.AgentUUID, addrs []string, now time.Time) (string, []any) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(addrs)), ",")
	args := make([]any, 0, len(addrs)+2)
	args = append(args, string(reader))
	for _, a := range addrs {
		args = append(args, a)
	}
	args = append(args, now.UnixNano(), string(reader))
	// from_uuid != reader: a sender's own broadcast (@all / @<slug>) must not
	// land back in their inbox — it would be permanent unread noise.
	q := `
SELECT m.id, m.from_uuid, m.from_handle, m.to_addr, m.body, m.thread_id, m.created_at, m.expires_at
FROM messages m
LEFT JOIN message_reads r ON r.message_id = m.id AND r.reader_uuid = ?
WHERE m.to_addr IN (` + placeholders + `)
  AND r.read_at IS NULL
  AND m.expires_at > ?
  AND m.from_uuid != ?
ORDER BY m.created_at, m.id`
	return q, args
}

// MarkRead stamps a per-reader read cursor on every message Inbox would
// currently return for (reader, addrs). Idempotent: INSERT OR IGNORE keeps a
// second mark from failing on the PK.
func (b *Box) MarkRead(ctx context.Context, reader domain.AgentUUID, addrs []string) error {
	if len(addrs) == 0 {
		return nil
	}
	msgs, err := b.Inbox(ctx, reader, addrs)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op
	for i := range msgs {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO message_reads(message_id,reader_uuid,read_at) VALUES (?,?,?)`,
			msgs[i].ID, string(reader), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Summary returns the unread count and deduplicated sender handles (fallback:
// short UUID) for the banner line, oldest sender first.
func (b *Box) Summary(ctx context.Context, reader domain.AgentUUID, addrs []string) (int, []string, error) {
	msgs, err := b.Inbox(ctx, reader, addrs)
	if err != nil {
		return 0, nil, err
	}
	var senders []string
	seen := map[string]bool{}
	for i := range msgs {
		name := msgs[i].FromHandle
		if name == "" {
			name = shortUUID(string(msgs[i].FromUUID))
		}
		if !seen[name] {
			seen[name] = true
			senders = append(senders, name)
		}
	}
	return len(msgs), senders, nil
}

func shortUUID(u string) string {
	if len(u) > 8 {
		return u[:8]
	}
	return u
}

// newMsgID mints a unique opaque message ID. crypto/rand failing on a working
// OS is catastrophic and not recoverable by the caller — panic rather than
// silently mint a predictable ID (same policy as the store's newID).
func newMsgID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return "m-" + hex.EncodeToString(b[:])
}
