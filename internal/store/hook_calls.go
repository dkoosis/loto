package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"loto/internal/domain"
)

// hookCallsDDL is the call-record layer the tree hooks write: one row per
// harness tool call, one row per path that call observed, and the per-path
// transition sequence both are keyed to (enforcement-design.md §3 "call" and
// "seq", §5 I3 step 4 and I4 step 5).
//
// Duplicated from schema.sql per the claimsDDL / violationsDDL precedent: a
// pending ensureFn must be able to apply itself. No user_version bump —
// bumping trips the move-aside path and would destroy live locks (loto-kwlp).
//
// ‡ The three tables are created by ONE ensure step, in one transaction, so
// probing for hook_calls alone is a sound probe for all three: SQLite cannot
// commit half of them.
const hookCallsDDL = `
CREATE TABLE IF NOT EXISTS hook_calls (
  call_id      TEXT PRIMARY KEY,
  owner_uuid   TEXT NOT NULL,
  session_uuid TEXT NOT NULL DEFAULT '',
  tool_name    TEXT NOT NULL DEFAULT '',
  t_pre        INTEGER NOT NULL,
  t_post       INTEGER,
  post_missing INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_hook_calls_inflight ON hook_calls(t_post, t_pre);

CREATE TABLE IF NOT EXISTS hook_call_paths (
  call_id        TEXT NOT NULL,
  path_canonical TEXT NOT NULL,
  locked         INTEGER NOT NULL DEFAULT 0,
  declared       INTEGER NOT NULL DEFAULT 0,
  pre_observed   INTEGER NOT NULL DEFAULT 1,
  epoch_pre      INTEGER NOT NULL DEFAULT 0,
  holder_pre     TEXT NOT NULL DEFAULT '',
  seq_pre        INTEGER NOT NULL DEFAULT 0,
  seq_post       INTEGER,
  digest_pre     TEXT NOT NULL DEFAULT '',
  digest_post    TEXT,
  stat_pre       TEXT NOT NULL DEFAULT '',
  stat_post      TEXT,
  PRIMARY KEY (call_id, path_canonical)
);

CREATE TABLE IF NOT EXISTS path_seq (
  path_canonical TEXT NOT NULL,
  epoch          INTEGER NOT NULL,
  seq            INTEGER NOT NULL,
  PRIMARY KEY (path_canonical, epoch)
);`

// ensureHookCallsTables creates the call-record layer in an existing DB that
// predates it. Pending when hook_calls is absent; a no-op on a fresh DB (where
// schemaSQL already declared all three) and on every re-Open.
func ensureHookCallsTables(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='hook_calls'`,
	).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if apply {
		if _, err := db.ExecContext(ctx, hookCallsDDL); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// ErrUnknownCall reports a post for a call_id no pre ever recorded. Distinct
// from "already posted": one is a hook that never saw the call's opening, the
// other is the idempotent repeat the design requires to be a no-op.
var ErrUnknownCall = errors.New("store: no call record for this call_id")

// HookCall is one harness tool call: (s, call_id, t_pre, t_post) from
// enforcement-design.md §3. TPost zero means the call is still in flight.
type HookCall struct {
	CallID      string
	OwnerUUID   domain.AgentUUID
	SessionUUID domain.SessionUUID
	ToolName    string
	TPre        time.Time
	TPost       time.Time
	// PostMissing: a live session's call whose post has not landed within
	// T_report. It changes what is REPORTED and nothing about contention —
	// a post_missing call is still in flight (§3, round 10).
	PostMissing bool
}

// InFlight reports whether this call's post has not landed. post_missing does
// not end a call: the two are independent, and reading them as alternatives is
// exactly the round-10 defect.
func (c HookCall) InFlight() bool { return c.TPost.IsZero() }

// HookPathState is one path as a hook observed it, at pre or at post. No
// bytes: stat and digest only (§5 I3 step 4, "No bytes").
type HookPathState struct {
	Path string
	// Locked: some live lock row covered this path when the hook looked.
	Locked bool
	// Declared: the Edit-family file_path this call was admitted on (I2).
	// Declaring is what lets a later bead exonerate a peer that said where
	// it was writing.
	Declared bool
	// Epoch and Holder are the lock's fencing generation and owner at pre.
	// Holder empty is ⊥ — unlocked, which is a legitimate recorded state:
	// a dirty unlocked path is what the spanning test reads for rows 7 and 8.
	Epoch  int64
	Holder domain.AgentUUID
	// Digest is the worktree content hash (git's own blob hash), empty when
	// the path does not exist. Stat is the cheap corroborator.
	Digest string
	Stat   string
}

// HookCallPath is one recorded path of a call, read back.
type HookCallPath struct {
	Path        string
	Locked      bool
	Declared    bool
	PreObserved bool
	Epoch       int64
	Holder      domain.AgentUUID
	SeqPre      int64
	SeqPost     int64
	HasPost     bool
	DigestPre   string
	DigestPost  string
	StatPre     string
	StatPost    string
}

// PostOutcome reports what a post did. Recorded false with a nil error is the
// idempotent repeat: a second post for one call_id changes nothing.
type PostOutcome struct {
	Recorded bool
	// Changed is the paths that took a new seq across this call, sorted.
	Changed []string
}

// pathSeqAt reads the current transition number of (path, epoch). Zero for a
// path that has never transitioned — a real value, not a sentinel: the first
// recorded change takes 1.
func pathSeqAt(ctx context.Context, tx *sql.Tx, path string, epoch int64) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx,
		`SELECT seq FROM path_seq WHERE path_canonical = ? AND epoch = ?`, path, epoch).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// nextPathSeq assigns the next transition number of (path, epoch) in the
// caller's tx, so the assignment and the record that cites it commit together.
// Same shape as nextPathEpoch, and for the same reason.
func nextPathSeq(ctx context.Context, tx *sql.Tx, path string, epoch int64) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO path_seq(path_canonical, epoch, seq) VALUES (?, ?, 1)
ON CONFLICT(path_canonical, epoch) DO UPDATE SET seq = seq + 1
RETURNING seq`, path, epoch).Scan(&seq)
	return seq, err
}

// PathSeq reads (f, E)'s transition number. The one read a caller outside this
// file needs; everything that ADVANCES it does so inside a write tx here.
func (s *Store) PathSeq(ctx context.Context, path string, epoch int64) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx,
		`SELECT seq FROM path_seq WHERE path_canonical = ? AND epoch = ?`, path, epoch).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// RecordCallPre writes the call and every path it observed, in ONE write
// transaction (§5 I3 step 4 — the same transaction `loto restore` will later
// take, which is why it is one and not several).
//
// Returns false when call_id is already on record: a repeated pre is a no-op
// for the same reason a repeated post is, and the harness can deliver one
// (a retried hook, a replayed event). The first pre wins, because it is the
// one whose observation actually preceded the tool.
func (s *Store) RecordCallPre(ctx context.Context, call HookCall, obs []HookPathState) (bool, error) {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return false, err
	}
	defer cleanup()

	res, err := tx.ExecContext(ctx, `
INSERT INTO hook_calls(call_id, owner_uuid, session_uuid, tool_name, t_pre, t_post, post_missing)
VALUES (?, ?, ?, ?, ?, NULL, 0)
ON CONFLICT(call_id) DO NOTHING`,
		call.CallID, string(call.OwnerUUID), string(call.SessionUUID), call.ToolName, call.TPre.UnixNano())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}

	for _, o := range obs {
		seqPre, err := pathSeqAt(ctx, tx, o.Path, o.Epoch)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO hook_call_paths(call_id, path_canonical, locked, declared, pre_observed,
                            epoch_pre, holder_pre, seq_pre, digest_pre, stat_pre)
VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?)`,
			call.CallID, o.Path, boolInt(o.Locked), boolInt(o.Declared),
			o.Epoch, string(o.Holder), seqPre, o.Digest, o.Stat); err != nil {
			return false, err
		}
	}
	if err := commitTxFn(tx); err != nil {
		return false, err
	}
	return true, nil
}

// RecordCallPost closes a call: seq_post and the post digest of every path it
// observed, and the next seq(f) for each path whose state changed across the
// call (§5 I4 step 5, record half).
//
// obs is the post observation set — the caller's union of the paths recorded
// at pre and the paths `git status` lists now, so a path that ENTERED or LEFT
// the status set inside the call is an observation like any other.
//
// Idempotent on call_id: a second post reports Recorded=false and writes
// nothing, including no second seq advance. An unknown call_id returns
// ErrUnknownCall — a post whose pre never landed is a fact worth surfacing,
// not a silent insert of a call nobody observed the opening of.
func (s *Store) RecordCallPost(ctx context.Context, callID string, tPost time.Time, obs []HookPathState) (PostOutcome, error) {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return PostOutcome{}, err
	}
	defer cleanup()

	var postNs sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT t_post FROM hook_calls WHERE call_id = ?`, callID).Scan(&postNs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PostOutcome{}, fmt.Errorf("%w: %s", ErrUnknownCall, callID)
		}
		return PostOutcome{}, err
	}
	if postNs.Valid {
		return PostOutcome{}, nil // already posted: no-op
	}

	recorded, err := recordedPathsTx(ctx, tx, callID)
	if err != nil {
		return PostOutcome{}, err
	}

	var changed []string
	for i := range obs {
		didChange, err := postOnePathTx(ctx, tx, callID, obs[i], recorded)
		if err != nil {
			return PostOutcome{}, err
		}
		if didChange {
			changed = append(changed, obs[i].Path)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE hook_calls SET t_post = ?, post_missing = 0 WHERE call_id = ?`,
		tPost.UnixNano(), callID); err != nil {
		return PostOutcome{}, err
	}
	if err := commitTxFn(tx); err != nil {
		return PostOutcome{}, err
	}
	sort.Strings(changed)
	return PostOutcome{Recorded: true, Changed: changed}, nil
}

// postOnePathTx writes one path's post half and reports whether it changed.
//
// Two shapes, kept apart on purpose: a path the pre recorded is UPDATEd in
// place, and a path first seen at post is INSERTed with an empty pre digest.
// Folding them would have to invent a pre-state for the second, which is the
// one thing an observation cannot do — nothing looked at that path before the
// tool ran.
func postOnePathTx(ctx context.Context, tx *sql.Tx, callID string, o HookPathState, recorded map[string]HookCallPath) (bool, error) {
	prev, wasRecorded := recorded[o.Path]
	// A path first seen at post entered the status set inside the call — that
	// IS the state change. A recorded path changed iff its content or its stat
	// moved; the digest is the authority and stat is the corroborator.
	didChange := !wasRecorded || prev.DigestPre != o.Digest || prev.StatPre != o.Stat
	epoch := o.Epoch
	if wasRecorded {
		epoch = prev.Epoch
	}
	seqPost := prev.SeqPre
	if didChange {
		var err error
		if seqPost, err = nextPathSeq(ctx, tx, o.Path, epoch); err != nil {
			return false, err
		}
	}
	if wasRecorded {
		_, err := tx.ExecContext(ctx, `
UPDATE hook_call_paths SET seq_post = ?, digest_post = ?, stat_post = ?
 WHERE call_id = ? AND path_canonical = ?`,
			seqPost, o.Digest, o.Stat, callID, o.Path)
		return didChange, err
	}
	seqPre, err := pathSeqAt(ctx, tx, o.Path, epoch)
	if err != nil {
		return false, err
	}
	// seqPre is read AFTER this path's own advance, so back it out: the number
	// the call started from is the one before its own transition.
	if seqPre > 0 {
		seqPre--
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO hook_call_paths(call_id, path_canonical, locked, declared, pre_observed,
                            epoch_pre, holder_pre, seq_pre, seq_post,
                            digest_pre, digest_post, stat_pre, stat_post)
VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, '', ?, '', ?)`,
		callID, o.Path, boolInt(o.Locked), boolInt(o.Declared),
		epoch, string(o.Holder), seqPre, seqPost, o.Digest, o.Stat)
	return didChange, err
}

// recordedPathsTx loads a call's pre-recorded paths, keyed by path.
func recordedPathsTx(ctx context.Context, tx *sql.Tx, callID string) (map[string]HookCallPath, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT path_canonical, locked, declared, pre_observed, epoch_pre, holder_pre,
       seq_pre, seq_post, digest_pre, digest_post, stat_pre, stat_post
  FROM hook_call_paths WHERE call_id = ?`, callID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]HookCallPath{}
	for rows.Next() {
		p, err := scanCallPath(rows)
		if err != nil {
			return nil, err
		}
		out[p.Path] = p
	}
	return out, rows.Err()
}

func scanCallPath(rows *sql.Rows) (HookCallPath, error) {
	var (
		p                             HookCallPath
		locked, declared, preObserved int
		holder                        string
		seqPost                       sql.NullInt64
		digestPost, statPost          sql.NullString
	)
	if err := rows.Scan(&p.Path, &locked, &declared, &preObserved, &p.Epoch, &holder,
		&p.SeqPre, &seqPost, &p.DigestPre, &digestPost, &p.StatPre, &statPost); err != nil {
		return HookCallPath{}, err
	}
	p.Locked = locked != 0
	p.Declared = declared != 0
	p.PreObserved = preObserved != 0
	p.Holder = domain.AgentUUID(holder)
	p.SeqPost, p.HasPost = seqPost.Int64, seqPost.Valid
	p.DigestPost = digestPost.String
	p.StatPost = statPost.String
	return p, nil
}

// CallRecord reads one call and its paths, paths sorted. ok=false when the
// call_id is unknown — dropped by retention, or never recorded.
func (s *Store) CallRecord(ctx context.Context, callID string) (call HookCall, paths []HookCallPath, ok bool, err error) {
	call, err = s.scanCall(ctx, `SELECT `+hookCallCols+` FROM hook_calls WHERE call_id = ?`, callID)
	if errors.Is(err, sql.ErrNoRows) {
		return HookCall{}, nil, false, nil
	}
	if err != nil {
		return HookCall{}, nil, false, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT path_canonical, locked, declared, pre_observed, epoch_pre, holder_pre,
       seq_pre, seq_post, digest_pre, digest_post, stat_pre, stat_post
  FROM hook_call_paths WHERE call_id = ? ORDER BY path_canonical`, callID)
	if err != nil {
		return HookCall{}, nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanCallPath(rows)
		if err != nil {
			return HookCall{}, nil, false, err
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return HookCall{}, nil, false, err
	}
	return call, paths, true, nil
}

const hookCallCols = `call_id,owner_uuid,session_uuid,tool_name,t_pre,t_post,post_missing`

func (s *Store) scanCall(ctx context.Context, query string, args ...any) (HookCall, error) {
	var (
		c              HookCall
		owner, session string
		preNs          int64
		postNs         sql.NullInt64
		missing        int
	)
	err := s.db.QueryRowContext(ctx, query, args...).
		Scan(&c.CallID, &owner, &session, &c.ToolName, &preNs, &postNs, &missing)
	if err != nil {
		return HookCall{}, err
	}
	c.OwnerUUID = domain.AgentUUID(owner)
	c.SessionUUID = domain.SessionUUID(session)
	c.TPre = time.Unix(0, preNs)
	if postNs.Valid {
		c.TPost = time.Unix(0, postNs.Int64)
	}
	c.PostMissing = missing != 0
	return c, nil
}

// InFlightCalls returns every call whose post has not landed, oldest first.
// post_missing calls are included: age never ends a call (§3).
func (s *Store) InFlightCalls(ctx context.Context) ([]HookCall, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+hookCallCols+` FROM hook_calls WHERE t_post IS NULL ORDER BY t_pre, call_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HookCall
	for rows.Next() {
		var (
			c              HookCall
			owner, session string
			preNs          int64
			postNs         sql.NullInt64
			missing        int
		)
		if err := rows.Scan(&c.CallID, &owner, &session, &c.ToolName, &preNs, &postNs, &missing); err != nil {
			return nil, err
		}
		c.OwnerUUID = domain.AgentUUID(owner)
		c.SessionUUID = domain.SessionUUID(session)
		c.TPre = time.Unix(0, preNs)
		c.PostMissing = missing != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkPostMissing flags every in-flight call older than tReport. It does NOT
// close them: a post_missing call still spans every transition inside its open
// interval, because a command that has not posted may still be running and may
// write next (§3, round 10). Returns how many rows the sweep flagged.
func (s *Store) MarkPostMissing(ctx context.Context, now time.Time, tReport time.Duration) (int, error) {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	res, err := tx.ExecContext(ctx,
		`UPDATE hook_calls SET post_missing = 1 WHERE t_post IS NULL AND post_missing = 0 AND t_pre < ?`,
		now.Add(-tReport).UnixNano())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := commitTxFn(tx); err != nil {
		return 0, err
	}
	return int(n), nil
}

// DropFinishedCalls applies §3's retention: a finished call's record is kept
// while any in-flight call has t_pre < its t_post, and dropped after.
//
// ‡ floor is this implementation's one addition to that sentence, and it only
// ever keeps MORE. Read literally, a post landing with nothing else in flight
// would delete its own record inside the same second it was written — before
// any reader (a peer's next pre-hook, `doctor`'s self-test, this bead's own
// acceptance) could see the pair. floor holds a finished record for that long
// regardless; T_report is what the caller passes, since that is already the
// window inside which a peer is expected to have looked.
func (s *Store) DropFinishedCalls(ctx context.Context, now time.Time, floor time.Duration) (int, error) {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	res, err := tx.ExecContext(ctx, `
DELETE FROM hook_calls
 WHERE t_post IS NOT NULL
   AND t_post < ?
   AND NOT EXISTS (
     SELECT 1 FROM hook_calls peer
      WHERE peer.t_post IS NULL AND peer.t_pre < hook_calls.t_post)`,
		now.Add(-floor).UnixNano())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM hook_call_paths WHERE call_id NOT IN (SELECT call_id FROM hook_calls)`); err != nil {
		return 0, err
	}
	if err := commitTxFn(tx); err != nil {
		return 0, err
	}
	return int(n), nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
