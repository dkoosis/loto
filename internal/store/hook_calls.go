package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
  post_missing INTEGER NOT NULL DEFAULT 0,
  dead_at      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_hook_calls_inflight ON hook_calls(t_post, dead_at, t_pre);

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
  digest         TEXT NOT NULL DEFAULT '',
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

// ensureHookCallsDeadAt adds hook_calls.dead_at to a DB created before the
// column existed. hook_calls has never shipped in a release, so the only DBs
// this can find are ones a developer built from an earlier commit of this
// branch — and for those the alternative is every store command dying on
// "no such column". Guarded and idempotent like every other ensure step.
func ensureHookCallsDeadAt(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('hook_calls') WHERE name = 'dead_at'`,
	).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if apply {
		if _, err := db.ExecContext(ctx, `ALTER TABLE hook_calls ADD COLUMN dead_at INTEGER`); err != nil {
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
	// DeadAt: when the liveness probe found this call's owner dead. §3 gives a
	// call exactly two ends — "until either post event lands OR the liveness
	// probe finds s dead" — and this is the second one. Kept distinct from
	// TPost rather than folded into it: the call never posted, and a later
	// reader judging what it spans has to be able to tell the difference.
	DeadAt time.Time
	// PostMissing: a live session's call whose post has not landed within
	// T_report. It changes what is REPORTED and nothing about contention —
	// a post_missing call is still in flight (§3, round 10).
	PostMissing bool
}

// InFlight reports whether this call is still open: no post landed and its
// owner is not known dead.
//
// ‡ post_missing does NOT appear here. Age never ends a call — reading
// post_missing as an ending is exactly the round-10 defect. A dead owner does
// end one, because a process that no longer exists cannot write next.
func (c HookCall) InFlight() bool { return c.TPost.IsZero() && c.DeadAt.IsZero() }

// Ended reports when this call stopped being in flight, zero while it is.
func (c HookCall) Ended() time.Time {
	if !c.TPost.IsZero() {
		return c.TPost
	}
	return c.DeadAt
}

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
	// SeqAtObserve is seq(f, E) read at the START of the observation, before
	// any stat or digest, and SeqAtObserveKnown says the caller set it.
	//
	// ‡ This closes a real exoneration hole. The digest is read OUTSIDE the
	// record transaction and seq_pre INSIDE it. A peer posting in that gap
	// assigns transition n; this call would then record seq_pre = n beside a
	// digest taken BEFORE n, and the spanning test `seq_pre < n` would read
	// false — the call is exonerated from a transition it actually spans.
	// Recording min(SeqAtObserve, the in-tx value) pins seq_pre to the number
	// that was true when the bytes were read. seq only ever rises, so the
	// minimum is the earlier read, and the error can only be conservative:
	// the call may span a transition it did not, never miss one it did.
	SeqAtObserve      int64
	SeqAtObserveKnown bool
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
	// Events is one entry per state change the post filed (§5 I4 step 5),
	// each carrying its spanners and the rule its report was written under.
	Events []TreeEvent
	// Acted is every path this call put back to a digest a report had named,
	// within TreeActedWindow — §10b row 2's numerator, sorted.
	Acted []string
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
// Same shape as nextPathEpoch, and for the same reason. digest is what the
// path is at the new number — see pathSeqStateTx for the one question it
// answers.
func nextPathSeq(ctx context.Context, tx *sql.Tx, path string, epoch int64, digest string) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO path_seq(path_canonical, epoch, seq, digest) VALUES (?, ?, 1, ?)
ON CONFLICT(path_canonical, epoch) DO UPDATE SET seq = seq + 1, digest = excluded.digest
RETURNING seq`, path, epoch, digest).Scan(&seq)
	return seq, err
}

// pathSeqStateTx reads (path, epoch)'s current number and the digest it was
// assigned at. known=false for a path that has never transitioned.
//
// ‡ The digest here answers exactly one question: "is the state I am looking
// at ALREADY numbered?" Two hooks can observe one physical change — the peer
// that wrote it posts, then the holder's own overlapping call posts and sees
// the same new bytes — and giving that one change two transition numbers
// breaks §10a test 1: the holder's event would be judged against a number no
// peer's call can span, and the peer that actually overlapped disappears from
// the report. Reusing the number when the current state is the numbered state
// puts both events on one transition, where the seq interval names them both.
//
// It is NOT the contention test. Whether a call spans a transition is decided
// by `seq_pre < n` and (`n <= seq_post` or unposted) and by nothing else; a
// digest comparison there is the round-9 hole (§3, §10a test 12's Fail line).
func pathSeqStateTx(ctx context.Context, tx *sql.Tx, path string, epoch int64) (seq int64, digest string, known bool, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT seq, digest FROM path_seq WHERE path_canonical = ? AND epoch = ?`,
		path, epoch).Scan(&seq, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return seq, digest, true, nil
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
		// The observation's own read wins when it is lower: seq_pre must name
		// the number that held when the digest beside it was taken, not one a
		// peer assigned in the gap. See HookPathState.SeqAtObserve.
		if o.SeqAtObserveKnown && o.SeqAtObserve < seqPre {
			seqPre = o.SeqAtObserve
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

	// loto-wkul: refresh the caller's own locks that have drifted under half
	// TTL, in this same transaction — the automatic heartbeat DESIGN.md:223
	// anticipated, wired to the one caller that costs the agent nothing.
	// RecordCallPre already runs on every write-capable tool call and already
	// holds this transaction, so folding the refresh in here is what keeps it
	// to one UPDATE and no second round trip.
	if err := refreshCallerLocksTx(ctx, tx, string(call.OwnerUUID), call.TPre); err != nil {
		return false, err
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
	var ownerStr, sessionStr string
	var postMissing int
	var tPreNs int64
	if err := tx.QueryRowContext(ctx,
		`SELECT t_post, owner_uuid, session_uuid, post_missing, t_pre FROM hook_calls WHERE call_id = ?`, callID,
	).Scan(&postNs, &ownerStr, &sessionStr, &postMissing, &tPreNs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PostOutcome{}, fmt.Errorf("%w: %s", ErrUnknownCall, callID)
		}
		return PostOutcome{}, err
	}
	// The observer is the call's OWN owner, read back rather than passed in:
	// an event names who was watching, and the only sound source for that is
	// the record the pre-hook wrote.
	owner := domain.AgentUUID(ownerStr)
	if postNs.Valid {
		return PostOutcome{}, nil // already posted: no-op
	}
	// §10b row 4's resolution half: this call was flagged post_missing by an
	// earlier MarkPostMissing sweep, and its post has now landed — late, but
	// it landed. Written in the same tx as the t_post update below, so the
	// pair (missing, resolved) commits atomically with the state it reports.
	if postMissing != 0 {
		if err := appendPostMissingResolvedTx(ctx, tx, callID, owner, domain.SessionUUID(sessionStr),
			"posted", tPost.Sub(time.Unix(0, tPreNs)), tPost); err != nil {
			return PostOutcome{}, err
		}
	}

	recorded, err := recordedPathsTx(ctx, tx, callID)
	if err != nil {
		return PostOutcome{}, err
	}

	var out PostOutcome
	for i := range obs {
		if err := postOnePathWithEventTx(ctx, tx, callID, owner, obs[i], recorded, tPost, &out); err != nil {
			return PostOutcome{}, err
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
	out.Recorded = true
	sort.Strings(out.Changed)
	sort.Strings(out.Acted)
	return out, nil
}

// postOnePathWithEventTx is one observed path's whole post: its record half,
// the acted counter, observed(f), and the event a state change files. Split
// out of RecordCallPost so that function stays a transaction and a loop.
func postOnePathWithEventTx(ctx context.Context, tx *sql.Tx, callID string, owner domain.AgentUUID,
	o HookPathState, recorded map[string]HookCallPath, tPost time.Time, out *PostOutcome,
) error {
	res, err := postOnePathTx(ctx, tx, callID, o, recorded)
	if err != nil {
		return err
	}
	// Acted BEFORE the event this path is about to file: the question is
	// whether the digest now on disk is one an EARLIER delivered report named,
	// and asking it first keeps the two passes independent of each other.
	acted, err := markTreeChangeActedTx(ctx, tx, owner, o.Path, o.Digest, tPost)
	if err != nil {
		return err
	}
	out.Acted = append(out.Acted, acted...)
	// observed(f) := the post state, for every locked path (§5 I4 step 5).
	// Without this the next pre-hook would read the pre-call state as the last
	// thing anyone saw and report this call's own write as drift.
	if o.Locked {
		if err := setObservedTx(ctx, tx, o, tPost); err != nil {
			return err
		}
	}
	if !res.changed {
		return nil
	}
	out.Changed = append(out.Changed, o.Path)
	ev, err := fileTreeEventTx(ctx, tx, treeEventInput{
		Path: o.Path, EpochPre: res.epoch, HolderPre: res.holderPre,
		EpochNow: o.Epoch, HolderNow: o.Holder,
		Observer: owner, CallID: callID, Seq: res.seqPost,
		DigestPre: res.digestPre, DigestPost: o.Digest, Declared: res.declared,
	}, tPost)
	if err != nil {
		return err
	}
	out.Events = append(out.Events, ev)
	return nil
}

// postPathResult is what one path's post half assigned, so RecordCallPost can
// file the event without re-reading the row it just wrote.
type postPathResult struct {
	changed   bool
	epoch     int64
	seqPost   int64
	digestPre string
	holderPre domain.AgentUUID
	declared  bool
}

// postOnePathTx writes one path's post half and reports whether it changed.
//
// Two shapes, kept apart on purpose: a path the pre recorded is UPDATEd in
// place, and a path first seen at post is INSERTed with an empty pre digest.
// Folding them would have to invent a pre-state for the second, which is the
// one thing an observation cannot do — nothing looked at that path before the
// tool ran.
func postOnePathTx(ctx context.Context, tx *sql.Tx, callID string, o HookPathState, recorded map[string]HookCallPath) (postPathResult, error) {
	prev, wasRecorded := recorded[o.Path]
	// A path first seen at post entered the status set inside the call — that
	// IS the state change. A recorded path changed iff its content or its stat
	// moved; the digest is the authority and stat is the corroborator.
	res := postPathResult{
		changed:   !wasRecorded || prev.DigestPre != o.Digest || prev.StatPre != o.Stat,
		epoch:     o.Epoch,
		digestPre: prev.DigestPre,
		holderPre: o.Holder,
		declared:  o.Declared,
	}
	if wasRecorded {
		res.epoch = prev.Epoch
		res.holderPre = prev.Holder
		res.declared = prev.Declared
	}
	curSeq, curDigest, _, err := pathSeqStateTx(ctx, tx, o.Path, res.epoch)
	if err != nil {
		return postPathResult{}, err
	}
	seqPre := prev.SeqPre
	if !wasRecorded {
		seqPre = curSeq
	}
	seqPost := seqPre
	switch {
	case !res.changed:
	// A change already numbered by whoever observed it first: this call files
	// its own event against THAT transition rather than inventing a second
	// number for one physical change. See pathSeqStateTx.
	case curSeq > seqPre && curDigest == o.Digest:
		seqPost = curSeq
	default:
		if seqPost, err = nextPathSeq(ctx, tx, o.Path, res.epoch, o.Digest); err != nil {
			return postPathResult{}, err
		}
	}
	res.seqPost = seqPost
	if wasRecorded {
		_, err := tx.ExecContext(ctx, `
UPDATE hook_call_paths SET seq_post = ?, digest_post = ?, stat_post = ?
 WHERE call_id = ? AND path_canonical = ?`,
			seqPost, o.Digest, o.Stat, callID, o.Path)
		return res, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO hook_call_paths(call_id, path_canonical, locked, declared, pre_observed,
                            epoch_pre, holder_pre, seq_pre, seq_post,
                            digest_pre, digest_post, stat_pre, stat_post)
VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, '', ?, '', ?)`,
		callID, o.Path, boolInt(o.Locked), boolInt(o.Declared),
		res.epoch, string(o.Holder), seqPre, seqPost, o.Digest, o.Stat)
	return res, err
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

const hookCallCols = `call_id,owner_uuid,session_uuid,tool_name,t_pre,t_post,post_missing,dead_at`

// hookCallInFlightSQL is the one spelling of "still in flight", named once so
// the sweep, the retention query and the in-flight read cannot drift apart.
// Both endings are here: a post landed, or the probe found the owner dead.
const hookCallInFlightSQL = `t_post IS NULL AND dead_at IS NULL`

// rowScanner is the one method *sql.Row and *sql.Rows share, so one row
// decoder serves the single-row read and the list.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanHookCall reads one hook_calls row in hookCallCols order.
func scanHookCall(sc rowScanner) (HookCall, error) {
	var (
		c              HookCall
		owner, session string
		preNs          int64
		postNs, deadNs sql.NullInt64
		missing        int
	)
	if err := sc.Scan(&c.CallID, &owner, &session, &c.ToolName, &preNs, &postNs, &missing, &deadNs); err != nil {
		return HookCall{}, err
	}
	c.OwnerUUID = domain.AgentUUID(owner)
	c.SessionUUID = domain.SessionUUID(session)
	c.TPre = time.Unix(0, preNs)
	if postNs.Valid {
		c.TPost = time.Unix(0, postNs.Int64)
	}
	if deadNs.Valid {
		c.DeadAt = time.Unix(0, deadNs.Int64)
	}
	c.PostMissing = missing != 0
	return c, nil
}

func (s *Store) scanCall(ctx context.Context, query string, args ...any) (HookCall, error) {
	return scanHookCall(s.db.QueryRowContext(ctx, query, args...))
}

// InFlightCalls returns every call that is still open, oldest first.
// post_missing calls are included: age never ends a call (§3). A call whose
// owner the probe found dead is NOT: that is §3's second ending.
func (s *Store) InFlightCalls(ctx context.Context) ([]HookCall, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+hookCallCols+` FROM hook_calls WHERE `+hookCallInFlightSQL+` ORDER BY t_pre, call_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HookCall
	for rows.Next() {
		c, err := scanHookCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkDeadOwnerCalls ends every in-flight call whose owner the liveness probe
// finds dead — §3's second ending, and the only thing that stops a crashed
// session's call from spanning every transition forever and pinning
// hook_calls against retention for good.
//
// dead is asked once per distinct session and must answer true ONLY for a
// PROVABLY dead session. An unknown verdict leaves the call in flight, which
// is loto's standing rule everywhere else: a false "gone" hands a live peer's
// territory away, while a false "alive" only delays a reclaim.
//
// The probe reads the filesystem, so it runs BETWEEN a read and a write rather
// than inside a write transaction. The UPDATE re-asserts the in-flight
// predicate, so a call that posted while the probe was running is untouched.
func (s *Store) MarkDeadOwnerCalls(ctx context.Context, now time.Time, dead func(domain.SessionUUID) bool) (int, error) {
	if dead == nil {
		return 0, nil
	}
	open, err := s.InFlightCalls(ctx)
	if err != nil {
		return 0, err
	}
	ids := deadOwnerCallIDs(open, dead)
	if len(ids) == 0 {
		return 0, nil
	}
	// byID lets markOneDeadCallTx read each call's PostMissing flag without a
	// second query — see its own doc comment for why that matters.
	byID := make(map[string]HookCall, len(open))
	for i := range open {
		byID[open[i].CallID] = open[i]
	}

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	var marked int
	for _, id := range ids {
		n, err := markOneDeadCallTx(ctx, tx, id, byID[id], now)
		if err != nil {
			return 0, err
		}
		marked += n
	}
	if err := commitTxFn(tx); err != nil {
		return 0, err
	}
	return marked, nil
}

// markOneDeadCallTx ends one in-flight call whose owner is dead, and if it
// was post_missing, resolves that too. Split out of MarkDeadOwnerCalls's loop
// so that function stays a plan (build the id list, then a tx) rather than
// growing a nested branch per row.
func markOneDeadCallTx(ctx context.Context, tx *sql.Tx, id string, c HookCall, now time.Time) (int, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE hook_calls SET dead_at = ? WHERE call_id = ? AND `+hookCallInFlightSQL,
		now.UnixNano(), id)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if c.PostMissing {
		// §10b row 4's other resolution: a call already flagged post_missing
		// whose owner turned out to be dead rather than slow. c comes from the
		// same InFlightCalls read the id list came from, not re-queried, so this
		// reads the PostMissing flag as it stood the moment the sweep decided
		// the owner was dead.
		if err := appendPostMissingResolvedTx(ctx, tx, id, c.OwnerUUID, c.SessionUUID,
			"session_died", now.Sub(c.TPre), now); err != nil {
			return 0, err
		}
	}
	return int(n), nil
}

// deadOwnerCallIDs picks the in-flight calls whose session the oracle calls
// dead, sorted. The oracle is asked once per distinct session: it reads a
// record off disk, and one crashed session usually owns several open calls.
func deadOwnerCallIDs(open []HookCall, dead func(domain.SessionUUID) bool) []string {
	verdict := map[domain.SessionUUID]bool{}
	var ids []string
	for i := range open {
		// A call with no session id can never be judged, so it is never ended
		// here; its owner may well be alive and about to post.
		sess := open[i].SessionUUID
		if sess == "" {
			continue
		}
		v, seen := verdict[sess]
		if !seen {
			v = dead(sess)
			verdict[sess] = v
		}
		if v {
			ids = append(ids, open[i].CallID)
		}
	}
	sort.Strings(ids)
	return ids
}

// postMissingDetail is the events.detail payload post_missing and
// post_missing_resolved both carry (§10b row 4): the call, its session, and
// an age in milliseconds — time since t_pre for post_missing, time from t_pre
// to resolution for post_missing_resolved. Which of the two happened rides in
// Event.Reason ("posted" or "session_died"), not in this struct.
type postMissingDetail struct {
	CallID  string `json:"call_id"`
	Session string `json:"session,omitempty"`
	AgeMS   int64  `json:"age_ms"`
}

// appendPostMissingResolvedTx writes one post_missing_resolved row: this
// call_id was flagged post_missing by an earlier MarkPostMissing sweep, and
// now either its post landed (reason "posted") or its owner was found dead
// (reason "session_died"). age is measured from t_pre, matching the age
// post_missing itself records, so a reader can compare when a call was first
// flagged against how long it eventually took to resolve.
func appendPostMissingResolvedTx(ctx context.Context, tx *sql.Tx, callID string, owner domain.AgentUUID,
	session domain.SessionUUID, reason string, age time.Duration, now time.Time,
) error {
	payload, err := json.Marshal(postMissingDetail{CallID: callID, Session: string(session), AgeMS: age.Milliseconds()})
	if err != nil {
		return err
	}
	return appendEventTx(ctx, tx, domain.Event{
		Kind:      EventPostMissingResolved,
		Target:    domain.Target{Canonical: callID},
		ActorUUID: string(owner),
		Reason:    reason,
		Detail:    string(payload),
		CreatedAt: now,
	})
}

// postMissingCandidate is one in-flight, not-yet-flagged call MarkPostMissing
// is about to flag: enough of its row to write both the UPDATE and the
// post_missing event without a second query per call.
type postMissingCandidate struct {
	callID, owner, session string
	tPre                   time.Time
}

// postMissingCandidatesTx reads every in-flight call whose t_pre is before
// cutoff and still has post_missing = 0 — MarkPostMissing's own WHERE clause,
// pulled out so that function stays a plan (read the candidates, then flag
// each) rather than a query and a branch inlined together.
func postMissingCandidatesTx(ctx context.Context, tx *sql.Tx, cutoff time.Time) ([]postMissingCandidate, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT call_id, owner_uuid, session_uuid, t_pre FROM hook_calls
		  WHERE `+hookCallInFlightSQL+` AND post_missing = 0 AND t_pre < ?
		  ORDER BY call_id`, cutoff.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []postMissingCandidate
	for rows.Next() {
		var c postMissingCandidate
		var tPreNs int64
		if err := rows.Scan(&c.callID, &c.owner, &c.session, &tPreNs); err != nil {
			return nil, err
		}
		c.tPre = time.Unix(0, tPreNs)
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkPostMissing flags every in-flight call older than tReport. It does NOT
// close them: a post_missing call still spans every transition inside its open
// interval, because a command that has not posted may still be running and may
// write next (§3, round 10). Returns how many rows the sweep flagged.
//
// One post_missing audit row is written per call flagged, in the same tx as
// the flag itself, so the counter §10b row 4 reads can never disagree with
// hook_calls.post_missing about which calls crossed T_report.
func (s *Store) MarkPostMissing(ctx context.Context, now time.Time, tReport time.Duration) (int, error) {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	toFlag, err := postMissingCandidatesTx(ctx, tx, now.Add(-tReport))
	if err != nil {
		return 0, err
	}
	if len(toFlag) == 0 {
		return 0, commitTxFn(tx)
	}

	for _, f := range toFlag {
		if _, err := tx.ExecContext(ctx,
			`UPDATE hook_calls SET post_missing = 1 WHERE call_id = ?`, f.callID); err != nil {
			return 0, err
		}
		payload, err := json.Marshal(postMissingDetail{CallID: f.callID, Session: f.session, AgeMS: now.Sub(f.tPre).Milliseconds()})
		if err != nil {
			return 0, err
		}
		if err := appendEventTx(ctx, tx, domain.Event{
			Kind:      EventPostMissing,
			Target:    domain.Target{Canonical: f.callID},
			ActorUUID: f.owner,
			Detail:    string(payload),
			CreatedAt: now,
		}); err != nil {
			return 0, err
		}
	}
	if err := commitTxFn(tx); err != nil {
		return 0, err
	}
	return len(toFlag), nil
}

// DropFinishedCalls applies §3's retention: a finished call's record is kept
// while any in-flight call has t_pre < the moment it ended, and dropped after.
//
// "Ended" is COALESCE(t_post, dead_at) — a call ends by posting or by its owner
// being found dead, and retention has to honour both or a crashed session's
// record makes the NOT EXISTS below true forever and the two tables grow
// without bound.
//
// ‡ floor is this implementation's one addition to §3's sentence, and it only
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
 WHERE COALESCE(t_post, dead_at) IS NOT NULL
   AND COALESCE(t_post, dead_at) < ?
   AND NOT EXISTS (
     SELECT 1 FROM hook_calls peer
      WHERE peer.t_post IS NULL AND peer.dead_at IS NULL
        AND peer.t_pre < COALESCE(hook_calls.t_post, hook_calls.dead_at))`,
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

// DeleteCallAndTiming removes one call's hook_calls/hook_call_paths rows and
// any hook_timing events (§10b row 3) whose detail names callID, in one
// transaction. Built for doctor's self-test leg (enforcement-design.md
// §10.3 rule 5, loto-ea8y.7): that leg proves the tree-hook plumbing works
// by actually recording a synthetic pre/post pair through the real hookPre/
// hookPost path, and a doctor run must leave the store exactly as it found
// it — ReadEnforcementStats (loto-ea8y.8) counts both tables, so a leftover
// row moves the count on every later run and breaks the byte-identical-
// output AC (PR #344 CI run 34360434734).
//
// The detail LIKE match is safe here specifically because callID is this
// process's own generated token (alnum and hyphens only, never a LIKE
// wildcard or quote) and hookEmitTiming always marshals CallID as the
// detail JSON's first field — a targeted cleanup for one known caller, not
// a general-purpose query.
//
// deletedCall reports whether a hook_calls row existed for callID at all —
// false is not an error: a pre-refusal writes no row, and cleanup must still
// run to be a no-op rather than fail.
func (s *Store) DeleteCallAndTiming(ctx context.Context, callID string) (deletedCall bool, deletedEvents int, err error) {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return false, 0, err
	}
	defer cleanup()

	res, err := tx.ExecContext(ctx, `DELETE FROM hook_calls WHERE call_id = ?`, callID)
	if err != nil {
		return false, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM hook_call_paths WHERE call_id = ?`, callID); err != nil {
		return false, 0, err
	}

	evRes, err := tx.ExecContext(ctx,
		`DELETE FROM events WHERE event_kind = ? AND detail LIKE ?`,
		EventHookTiming, `%"call_id":"`+callID+`"%`)
	if err != nil {
		return false, 0, err
	}
	evN, err := evRes.RowsAffected()
	if err != nil {
		return false, 0, err
	}

	if err := commitTxFn(tx); err != nil {
		return false, 0, err
	}
	return n > 0, int(evN), nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ensureTreeReportsActedAt adds tree_reports.acted_at to a DB created before
// the column existed. Same guarded, idempotent shape as ensurePathSeqDigest,
// and for the same reason: these tables have never shipped in a release.
func ensureTreeReportsActedAt(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('tree_reports') WHERE name = 'acted_at'`,
	).Scan(&n); err != nil {
		return false, err
	}
	// Absent table: ensureTreeEventsTables creates it with the column, and it
	// runs first. Nothing to add, and nothing pending.
	if n > 0 || !tableExists(ctx, db, "tree_reports") {
		return false, nil
	}
	if apply {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE tree_reports ADD COLUMN acted_at INTEGER`); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// tableExists is the sqlite_master probe the two ensure steps above share.
func tableExists(ctx context.Context, db sqlExecQuerier, name string) bool {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// ensurePathSeqDigest adds path_seq.digest to a DB created before the column
// existed. Same guarded, idempotent shape as ensureHookCallsDeadAt, and for
// the same reason: path_seq has never shipped in a release, so the only DBs
// this can find were built from an earlier commit of this branch, where the
// alternative is every store command dying on "no such column".
func ensurePathSeqDigest(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('path_seq') WHERE name = 'digest'`,
	).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if apply {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE path_seq ADD COLUMN digest TEXT NOT NULL DEFAULT ''`); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}
