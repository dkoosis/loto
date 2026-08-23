package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const schemaUserVersion = 9

var errUserVersionMismatch = errors.New("loto: schema user_version mismatch")

type Store struct {
	db     *sql.DB
	dbPath string
	stderr io.Writer
	// repoTop is the absolute checkout root that repo-relative canonicals are
	// probed against on the filesystem (loto-3tv3 D8). Empty means "no repo
	// frame": probes fall back to the canonical as given, which is the legacy
	// behavior and what the in-package tests (absolute canonicals) exercise.
	//
	// ‡ Per-process, not per-store-file. StateDir keys the DB by origin-remote
	// slug, so two worktrees of one repo share a store; repoTop means "validate
	// against MY checkout", the same semantics statFileTargetReason has had
	// since loto-cqk.
	repoTop string
}

// OpenOption configures a Store after its connection is established. Options
// are applied post-open so nothing threads through the acquireOpenLocks →
// openWithRecovery → openOnce chain.
type OpenOption func(*Store)

// WithRepoTop tells the store which checkout to resolve repo-relative
// canonicals against. Without it, validateFileTarget Lstats the canonical bare
// — i.e. against the process CWD — which is correct only when the process
// happens to be standing at the repo root.
func WithRepoTop(top string) OpenOption {
	return func(s *Store) { s.repoTop = top }
}

// connDSN: WAL + busy_timeout + immediate-mode write txns.
func connDSN(path string) string {
	return path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_txlock=immediate"
}

// Open opens the loto store at path. On a real sqlite corruption error
// (SQLITE_CORRUPT or SQLITE_NOTADB, errno-checked — not string-matched),
// the existing DB and its -wal/-shm siblings are moved aside atomically
// and a fresh DB is created. Recovery is serialized via flock on a
// sidecar lock file so concurrent openers cannot interleave.
//
// First-Open is serialized on the project op-flock: two processes
// creating the same DB simultaneously would otherwise both pass the
// existence check and clobber each other's writes. Subsequent Opens on
// an initialized DB take the fast path (no flock).
func Open(p string) (*Store, error) {
	return OpenContext(context.Background(), p) //nolint:forbidigo // Open is the no-context convenience root; OpenContext is the ctx-threaded path for callers that have one.
}

// OpenContext is Open with a caller-supplied context. Cancellation aborts
// flock polling (op-flock + recovery-lock) instead of waiting out
// LOTO_FLOCK_TIMEOUT.
func OpenContext(ctx context.Context, p string, opts ...OpenOption) (*Store, error) {
	s, err := acquireOpenLocks(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// acquireOpenLocks is the single canonical entry point for the Open-path
// lock dance. It enforces the gh#109 invariant:
//
//	op-flock is NEVER held across acquireRecoveryLock.
//
// Op-flock protects only the create-race window on fresh DBs (two
// concurrent first-Opens picking the same path). Recovery-lock serializes
// corrupt-DB recovery and is taken alone. Holding both at once would
// (a) be one missing rename away from an AB/BA deadlock with any future
// caller that takes them in the opposite order, and (b) stall every
// unrelated `loto` invocation for the full recovery poll window, since
// acquire/release/break/doctor all need op-flock.
//
// Canonical order, when both are needed by a single caller: op-flock
// first, then release before recovery-lock. The fresh-DB path here
// follows that rule by releasing op-flock immediately after the initial
// openOnce attempt — before any recovery-lock acquire — even when the
// initial attempt fails with corruption/version mismatch.
func acquireOpenLocks(ctx context.Context, p string) (*Store, error) {
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		// A non-empty file on disk is NOT proof the DB is usable: a
		// concurrent first-Open creates the file and begins writing the
		// schema/WAL well before it stamps user_version, so a peer that
		// stats here mid-create would see size>0 and, on the old gate,
		// skip op-flock straight into openWithRecovery — racing the
		// in-flight create/migrate (loto-qev1: SQLITE_IOERR 1802,
		// SQLITE_BUSY, or a user_version=0 mismatch that then triggered a
		// bogus move-aside).
		//
		// Gate the fast lock-free path on the DB being PROVABLY initialized
		// (user_version == schemaUserVersion), probed lock-free. Any probe
		// error — including the transient I/O/BUSY of a mid-create DB — is
		// treated as "not yet initialized" and falls through to the
		// op-flock-guarded path, which serializes behind the create.
		if dbInitialized(ctx, p) {
			// Steady state: DB already stamped. No create race possible,
			// so op-flock isn't needed. openWithRecovery may take
			// recovery-lock alone.
			return openWithRecovery(ctx, p)
		}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat db path: %w", err)
	}

	// Fresh-or-mid-create path: op-flock guards the create-race window, but
	// is released before any recovery-lock acquire (gh#109).
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	flock, err := acquireOpFlockFn(ctx, opFlockPathFor(p), os.Stderr)
	if err != nil {
		return nil, err
	}
	// Safety net for an unwind (panic) through the gap below: release()
	// nil-guards, so on the normal path this is a no-op once we've nulled
	// the handle's file after the explicit release. This must NOT replace
	// the explicit release at the end of the gap — the gh#109 invariant
	// requires op-flock be freed BEFORE any recovery-lock acquire, and a
	// bare defer would only fire after openWithRecovery returns.
	defer flock.release()
	s, openErr := openOnce(ctx, p)
	// Release op-flock BEFORE any recovery-lock acquire. If openOnce
	// succeeded the create race is resolved; if it failed with corruption
	// or version mismatch, openWithRecovery will retake recovery-lock
	// alone — never with op-flock held. After release the handle is
	// already nil-guarded; the deferred safety-net release above will
	// become a no-op.
	flock.release()
	if openErr == nil {
		return s, nil
	}
	if !isCorruptDB(openErr) && !isUserVersionMismatch(openErr) {
		return nil, openErr
	}
	return openWithRecovery(ctx, p)
}

// dbInitialized reports whether the DB at p is a fully-initialized loto store
// — i.e. a lock-free read sees user_version == schemaUserVersion. It is the
// gate for the fast lock-free Open path: only a stamped DB skips the op-flock.
//
// Crucially it is conservative on EVERY failure. A DB mid-create (concurrent
// first-Open) may transiently return SQLITE_IOERR(1802)/SQLITE_BUSY or a stale
// user_version=0; all of those return false here, routing the caller into the
// op-flock-guarded path where it serializes behind the in-flight create rather
// than racing it (loto-qev1). False negatives are cheap (one extra flock
// acquire on an already-good DB under contention); a false positive would
// reintroduce the race, so the bias is deliberate.
func dbInitialized(ctx context.Context, p string) bool {
	db, err := sql.Open("sqlite", connDSN(p))
	if err != nil {
		return false
	}
	defer db.Close()
	// No separate PingContext: QueryRowContext establishes the connection and
	// its error covers the same transient mid-create failures (IOERR/BUSY),
	// so a dedicated ping would only double the fast-path round-trips.
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return false
	}
	return v == schemaUserVersion
}

// opFlockPathFor returns the op-flock path for a DB at p — used during
// Open() before a *Store exists.
func opFlockPathFor(p string) string {
	return filepath.Join(filepath.Dir(p), "lock-op.flock")
}

func openWithRecovery(ctx context.Context, p string) (*Store, error) {
	s, err := openOnce(ctx, p)
	if err == nil {
		return s, nil
	}
	if !isCorruptDB(err) && !isUserVersionMismatch(err) {
		return nil, err
	}

	release, lockErr := acquireRecoveryLock(ctx, p)
	if lockErr != nil {
		return nil, fmt.Errorf("acquire recovery lock: %w (orig: %w)", lockErr, err)
	}
	defer release()

	// Re-probe under the lock — another process may have already recovered.
	if s2, err2 := openOnce(ctx, p); err2 == nil {
		return s2, nil
	} else if !isCorruptDB(err2) && !isUserVersionMismatch(err2) {
		return nil, err2
	}

	moved, mvErr := moveCorruptAside(p, time.Now())
	if mvErr != nil {
		return nil, fmt.Errorf("incompatible DB and move-aside failed: %w (orig: %w)", mvErr, err)
	}
	if isUserVersionMismatch(err) {
		fmt.Fprintf(os.Stderr, "loto: incompatible DB schema moved aside to %s; created fresh DB\n", moved)
	} else {
		fmt.Fprintf(os.Stderr, "loto: corrupt DB moved aside to %s; creating fresh DB\n", moved)
	}
	return openOnce(ctx, p)
}

// openOnceHook is a test seam fired at the very top of openOnce, inside the
// op-flock gap of the fresh-DB path. Nil in production. Tests set it to inject
// a panic and assert the op-flock is still released on unwind (loto-8yst).
var openOnceHook func()

func openOnce(ctx context.Context, p string) (*Store, error) {
	if openOnceHook != nil {
		openOnceHook()
	}
	preExisted := false
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		preExisted = true
	}

	db, err := sql.Open("sqlite", connDSN(p))
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	if preExisted {
		var v int
		if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
			db.Close()
			return nil, fmt.Errorf("read user_version: %w", err)
		}
		// A version mismatch only forces move-aside when the DB is genuinely
		// incompatible. A STALE version (below current) on a structurally-intact
		// loto schema is the loto-vmym window: a crash between the schema-tx
		// commit and the separate user_version PRAGMA write, or a DB created
		// before schemaUserVersion was bumped. Those re-migrate idempotently in
		// place (migrate re-stamps the version) rather than destroying live
		// locks. A FUTURE version (above current), or a foreign schema with no
		// `locks` table, is still moved aside.
		if v != schemaUserVersion && (v > schemaUserVersion || !schemaStructurallyIntact(ctx, db)) {
			db.Close()
			return nil, fmt.Errorf("%w: have %d, want %d", errUserVersionMismatch, v, schemaUserVersion)
		}
	}

	s := &Store{db: db, dbPath: p, stderr: os.Stderr}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// isCorruptDB returns true only for real sqlite errno results indicating
// an unreadable database file: SQLITE_CORRUPT (11) or SQLITE_NOTADB (26).
// The previous string-match implementation false-positived on any wrapped
// error containing "malformed" and destroyed healthy databases (gh#48).
// Primary code is masked off any extended-code bits per the sqlite spec.
func isCorruptDB(err error) bool {
	if err == nil {
		return false
	}
	var sqErr *sqlite.Error
	if !errors.As(err, &sqErr) {
		return false
	}
	primary := sqErr.Code() & 0xff
	return primary == sqlite3.SQLITE_CORRUPT || primary == sqlite3.SQLITE_NOTADB
}

func isUserVersionMismatch(err error) bool { return errors.Is(err, errUserVersionMismatch) }

// schemaStructurallyIntact reports whether the core loto `locks` table exists,
// the sentinel for "this is a loto DB with a stale version stamp" vs "a foreign
// or incompatibly-old DB". Used by the openOnce version gate (loto-vmym) to
// decide re-migrate-in-place over destructive move-aside. A probe failure is
// treated as not-intact (conservative: prefer move-aside on an unreadable DB).
func schemaStructurallyIntact(ctx context.Context, db *sql.DB) bool {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='locks'`).Scan(&name)
	return err == nil && name == "locks"
}

// sqlExecQuerier is the read+write surface an ensure step needs. Both *sql.DB
// (used by the read-only dry-run gate) and *sql.Tx (used by migrate's apply
// path) satisfy it, so one ensureFn body serves both callers.
type sqlExecQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ensureFn is one guarded, additive in-place migration. It probes (read-only)
// whether the DB already satisfies the step and returns pending=true when the
// upgrade is still outstanding. When apply is true and the step is pending it
// also executes the DDL. The probe is identical in both modes, so the same fn
// is the single source of truth for "is this step done?" — consulted by the
// steady-state gate (apply=false, no write tx) and the writer (apply=true).
type ensureFn func(ctx context.Context, db sqlExecQuerier, apply bool) (pending bool, err error)

// migrationEnsures are the additive in-place upgrades migrate applies after the
// base schemaSQL, in dependency order (proc_start before the locks rebuild,
// whose SELECT relies on the column existing). migrate's apply path and
// schemaCurrent's steady-state gate iterate THIS one list — the gate runs each
// step with apply=false, the apply path with apply=true — so a new step is
// impossible to add to one path without the other. This closes the drift
// loto-t8dd targeted: the old schemaFullyCurrent hand-mirrored each probe and
// could silently fall behind a newly added ensure, leaving a steady-state DB
// to skip an upgrade forever.
var migrationEnsures = []struct {
	name string
	fn   ensureFn
}{
	{"add locks.proc_start", ensureLocksProcStart},
	{"upgrade locks mode/pk", ensureLocksModeAndPK},
	{"add locks.beacon", ensureLocksBeacon},
	{"upgrade events check", ensureEventsCheckCurrent},
	{"add claims table", ensureClaimsTable},
	{"add territory_tags table", ensureTerritoryTagsTable},
	{"add locks.epoch", ensureLocksEpoch},
	{"add path_epochs table", ensurePathEpochsTable},
	{"add candidate_claims table", ensureCandidateClaimsTable},
	{"add violations table", ensureViolationsTable},
	{"add violations.baseline", ensureViolationsBaseline},
	{"add violations.worktree", ensureViolationsWorktree},
	{"scope violations open index to worktree", ensureViolationsOpenIndexScoped},
}

// schemaCurrent reports whether a re-migrate would be a pure no-op — the gate
// for migrate's steady-state no-write fast path (loto-0gsu). It is the dry-run
// counterpart of migrate's apply loop: every migrationEnsures step is probed
// with apply=false (read-only, no write tx) and the DB is current only when
// none is pending. Base-schema tables (locks via schemaStructurallyIntact,
// tags) are not ensure steps, so their existence is checked directly first;
// events existence is covered by ensureEventsCheckCurrent's own probe
// (ErrNoRows → pending). Any probe failure or pending step is treated as
// not-current (conservative: prefer running migrate). Unlike
// schemaStructurallyIntact — which only sentinels "is this a loto DB" for the
// move-aside decision — this must be true ONLY when migrate has nothing to do.
func schemaCurrent(ctx context.Context, db *sql.DB) bool {
	if !schemaStructurallyIntact(ctx, db) {
		return false
	}
	var tagsName string
	if err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='tags'`).Scan(&tagsName); err != nil || tagsName != "tags" {
		return false
	}
	for _, step := range migrationEnsures {
		pending, err := step.fn(ctx, db, false)
		if err != nil || pending {
			return false
		}
	}
	return true
}

func (s *Store) Close() error { return s.db.Close() }

// setStderr overrides the writer used for diagnostic messages (audit-write
// failures, op-flock contention notices). Defaults to os.Stderr. Intended for
// tests that need to observe these messages; production code should keep the
// default.
func (s *Store) setStderr(w io.Writer) { s.stderr = w }

// beginTx starts an immediate-mode tx on a dedicated pooled conn whose
// busy_timeout PRAGMA is scaled to the caller's ctx deadline. Returned
// cleanup MUST be deferred — it rolls back if Commit wasn't called and
// always releases the conn back to the pool. Rollback after Commit is a
// safe no-op (sql.ErrTxDone), so callers may unconditionally `defer cleanup()`.
//
// Rationale (gh#55): the DSN-level busy_timeout=5000 ignored caller ctx:
// short deadlines couldn't pre-empt SQLite's internal poll loop, and
// longer deadlines were silently truncated to 5s. Per-tx scaling restores
// the contract that ctx is authoritative.
// commitTxFn indirects tx.Commit so tests can simulate a commit failure
// (disk-full / SQLITE_IOERR) on a write path without a real I/O fault.
var commitTxFn = func(tx *sql.Tx) error { return tx.Commit() }

func (s *Store) beginTx(ctx context.Context) (*sql.Tx, func(), error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	timeoutMs := txBusyTimeoutMs(ctx, time.Now())
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", timeoutMs)); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	cleanup := func() {
		_ = tx.Rollback()
		// Reset busy_timeout to the DSN default before the conn returns to
		// the pool — otherwise the next caller inherits this caller's
		// ctx-scaled value (gh#55 follow-up).
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", txBusyTimeoutDefaultMs)) //nolint:forbidigo // teardown reset must run on conn return even if the request ctx is already cancelled (gh#55).
		_ = conn.Close()
	}
	return tx, cleanup, nil
}

// txBusyTimeoutMs maps ctx.Deadline → SQLite busy_timeout in ms.
// No deadline → fall back to DSN default (5000ms).
// Deadline already past → 1ms (caller will see ctx.Err() at next step).
// Otherwise → milliseconds remaining, clamped to [1, txBusyTimeoutCapMs].
func txBusyTimeoutMs(ctx context.Context, now time.Time) int {
	dl, ok := ctx.Deadline()
	if !ok {
		return txBusyTimeoutDefaultMs
	}
	rem := dl.Sub(now).Milliseconds()
	switch {
	case rem < 1:
		return 1
	case rem > txBusyTimeoutCapMs:
		return txBusyTimeoutCapMs
	default:
		return int(rem)
	}
}

const (
	txBusyTimeoutDefaultMs = 5000
	txBusyTimeoutCapMs     = 60000
)

// opFlockPath returns <db-dir>/lock-op.flock — the project-wide op-flock.
func (s *Store) opFlockPath() string {
	return filepath.Join(filepath.Dir(s.dbPath), "lock-op.flock")
}

// migrate applies schema DDL inside a transaction, then sets user_version
// in a separate statement. PRAGMA user_version is not transactional in
// SQLite (it takes effect immediately regardless of tx state), so it runs
// after the DDL tx commits. If a crash occurs between commit and PRAGMA,
// the schema is intact but user_version is stale; openOnce's gate detects
// that (stale version + present `locks` table) and routes the next Open back
// through this idempotent migrate, which re-stamps user_version (loto-vmym).
func (s *Store) migrate(ctx context.Context) error {
	// Steady-state fast path (loto-0gsu): if the DB is already at the current
	// version with an intact schema, do nothing. A redundant migrate here would
	// open an immediate-mode (write) tx that takes SQLite's WAL writer lock and
	// re-stamp user_version on every Open — even for read-only commands
	// (cmdCheck, cmdStatus) that reach openOnce → migrate. That serialized
	// concurrent reads on the writer lock and dirtied the DB on every read.
	// Both probes are read-only PRAGMA/SELECTs on the pool conn — no write tx.
	// The stale-but-intact case (loto-vmym crash window: version below target)
	// falls through and re-migrates in place, re-stamping user_version.
	var v int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if v == schemaUserVersion && schemaCurrent(ctx, s.db) {
		return nil
	}

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin migrate tx: %w", err)
	}
	defer cleanup()
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	// Apply every additive in-place upgrade in order. Each step is guarded and
	// idempotent (no-op on a fresh DB where schemaSQL already declared the
	// shape, and on every re-Open), so this is safe to run unconditionally
	// after schemaSQL. user_version is intentionally NOT bumped by any step —
	// bumping would trip the move-aside path and destroy live locks (loto-kwlp
	// precedent); these upgrades preserve existing rows. The dependency order
	// lives in migrationEnsures (proc_start before the locks rebuild). The same
	// list backs schemaCurrent's no-write gate, so the two cannot drift apart.
	for _, step := range migrationEnsures {
		if _, err := step.fn(ctx, tx, true); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	if err := commitTxFn(tx); err != nil {
		return fmt.Errorf("commit schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaUserVersion)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}

// ensureLocksProcStart adds the locks.proc_start column to an existing DB that
// predates it (loto-kwlp). Pending when the column is absent; not-pending on a
// fresh DB (CREATE TABLE already declared it) and on every re-Open. The probe
// is read-only, so apply=false makes this a pure predicate for schemaCurrent.
func ensureLocksProcStart(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE name = 'proc_start'`,
	).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if apply {
		if _, err := db.ExecContext(ctx, `ALTER TABLE locks ADD COLUMN proc_start INTEGER`); err != nil {
			return false, err
		}
		return false, nil // applied: no longer outstanding
	}
	return true, nil
}

// legacyBeaconIntent is the intent `loto beacon` stamped during the one release
// that minted beacons before locks.beacon existed. Frozen history, not a live
// coupling to internal/cli's beaconIntent — the arch layering forbids that
// import, and this literal must never change even if the CLI's does: it names
// rows already on disk. Sole use is the backfill below.
const legacyBeaconIntent = "beacon: agent is writing this file"

// ensureLocksBeacon adds the locks.beacon column to an existing DB that
// predates it (loto-dm4i). Pending when the column is absent; not-pending on a
// fresh DB (CREATE TABLE already declared it) and on every re-Open. Ordered
// AFTER ensureLocksModeAndPK: that step rebuilds a legacy locks table from a
// fixed column list which does not name beacon, so adding the column first
// would lose it again on the rebuild. user_version is not bumped: a bump trips
// MoveCorruptAside and destroys live locks (loto-kwlp precedent).
//
// ‡ The backfill matters because the default is the wrong answer for exactly
// one population (Codex #252). Rows taken before beacons existed were asked
// for by an agent, which is what 0 means — but the release immediately before
// this one DID mint beacons, as shared / pid-0 rows carrying a fixed intent.
// Defaulting those to 0 would promote them to apparent explicit leases that
// guard refuses to move past.
//
// The predicate names every persisted field a beacon fixes and a hand-taken
// lock does not: shared, pid 0, no branch, and that intent. Branch is in there
// because intent alone is user-supplied — `loto lock --shared -t "<the beacon
// text>"` is possible, if perverse — while buildLockRecords stamps the holder's
// git branch (loto-16cf) and buildBeaconRecords deliberately does not. Wrongly
// marking a real lease would let a tree move waive it, so the backfill errs
// toward leaving rows alone.
func ensureLocksBeacon(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE name = 'beacon'`,
	).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if apply {
		if _, err := db.ExecContext(ctx, `ALTER TABLE locks ADD COLUMN beacon INTEGER NOT NULL DEFAULT 0`); err != nil {
			return false, err
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE locks SET beacon = 1 WHERE mode = 'shared' AND pid = 0 AND branch = '' AND intent = ?`,
			legacyBeaconIntent,
		); err != nil {
			return false, err
		}
		return false, nil // applied: no longer outstanding
	}
	return true, nil
}

// ensureLocksEpoch adds locks.epoch to an existing DB that predates it
// (loto-ovno.2). Legacy rows default to 0 — indistinguishable from a
// first-ever grant at that path, which is the correct reading: nothing had
// captured a candidate envelope against a row that predates epochs existing.
func ensureLocksEpoch(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE name = 'epoch'`,
	).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if apply {
		if _, err := db.ExecContext(ctx, `ALTER TABLE locks ADD COLUMN epoch INTEGER NOT NULL DEFAULT 0`); err != nil {
			return false, err
		}
		return false, nil // applied: no longer outstanding
	}
	return true, nil
}

// ensurePathEpochsTable adds the durable per-path epoch counter to an
// existing DB (loto-ovno.2). Follows ensureClaimsTable's precedent: probe
// sqlite_master, apply the DDL, never bump user_version.
func ensurePathEpochsTable(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	return ensureTableBySentinelName(ctx, db, apply, "path_epochs", pathEpochsDDL)
}

// ensureCandidateClaimsTable adds the durable candidate-claim table to an
// existing DB (loto-ovno.2). Same precedent as ensurePathEpochsTable.
func ensureCandidateClaimsTable(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	return ensureTableBySentinelName(ctx, db, apply, "candidate_claims", candidateClaimsDDL)
}

// ensureViolationsTable adds the sticky-violation table to an existing DB
// (loto-ovno.9). Same precedent as ensurePathEpochsTable.
func ensureViolationsTable(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	return ensureTableBySentinelName(ctx, db, apply, "violations", violationsDDL)
}

// ensureViolationsBaseline adds the baseline column to a violations table
// created before that column existed. The sentinel-name ensure above probes
// only for the TABLE, so a DB carrying this table's first shape would keep it
// forever and every insert would fail on the missing column. Follows
// ensureLocksEpoch: probe pragma_table_info, ALTER, never bump user_version.
func ensureViolationsBaseline(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('violations') WHERE name = 'baseline'`,
	).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if apply {
		if _, err := db.ExecContext(ctx, `ALTER TABLE violations ADD COLUMN baseline TEXT NOT NULL DEFAULT ''`); err != nil {
			return false, err
		}
		return false, nil // applied: no longer outstanding
	}
	return true, nil
}

// ensureViolationsWorktree adds the worktree column to a violations table
// created before checkouts were distinguished (loto-nper). Same precedent as
// ensureViolationsBaseline: probe pragma_table_info, ALTER, never bump
// user_version.
//
// ‡ EVERY pre-existing row is moved to WorktreeLegacy, not left at the column
// default. A pre-upgrade scan could run from any checkout, so calling those
// rows "primary" asserts an origin the DB never recorded — wrong in both
// directions at once: a linked checkout stops seeing its own open violation
// (Codex #283 P1), and an ack it made starts suppressing the primary tree's
// flags (Codex #283 P2). Resolved rows are backfilled for exactly that second
// reason; see WorktreeLegacy for the rule the two share.
//
// The ALTER and the backfill run inside migrate's single tx, so no row is
// ever readable in the intermediate state.
func ensureViolationsWorktree(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	return ensureColumn(ctx, db, apply, "violations", "worktree",
		`ALTER TABLE violations ADD COLUMN worktree TEXT NOT NULL DEFAULT ''`,
		`UPDATE violations SET worktree = '`+WorktreeLegacy+`'`)
}

// ensureViolationsOpenIndexScoped widens the open-violation uniqueness key
// from (path_canonical) to (path_canonical, worktree).
//
// ‡ Must run AFTER ensureViolationsWorktree — the index it creates names that
// column. The old index is not merely redundant: while it stands, two
// checkouts of one repo cannot each hold an open row for the same path, so
// the second one's violation is silently dropped by RecordViolations' INSERT
// OR IGNORE and the contamination goes unrecorded. Dropping it is the whole
// migration; SQLite has no ALTER INDEX.
func ensureViolationsOpenIndexScoped(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_violations_open_path_wt'`).Scan(&name)
	if err == nil {
		return false, nil // already scoped
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if apply {
		if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_violations_open_path`); err != nil {
			return false, err
		}
		if _, err := db.ExecContext(ctx,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_violations_open_path_wt
			   ON violations(path_canonical, worktree) WHERE resolved_at IS NULL`); err != nil {
			return false, err
		}
		return false, nil // applied: no longer outstanding
	}
	return true, nil
}

// ensureColumn is the shared body ensureLocksEpoch / ensureViolationsBaseline
// each hand-duplicated: probe pragma_table_info for the column, run the
// caller's ALTER if absent, never touch user_version. Trailing statements run
// in the same apply: a backfill a new column needs is part of adding it, and
// splitting the two into separate steps would make the un-backfilled state
// reachable by a probe that then skips it.
func ensureColumn(ctx context.Context, db sqlExecQuerier, apply bool, table, column, alter string, after ...string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if apply {
		for _, stmt := range append([]string{alter}, after...) {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return false, err
			}
		}
		return false, nil // applied: no longer outstanding
	}
	return true, nil
}

// ensureTableBySentinelName is the shared body ensureClaimsTable /
// ensureTerritoryTagsTable each hand-duplicated: probe sqlite_master for
// tableName, apply ddl if apply and absent, never touch user_version. Factored
// out here rather than duplicated a third and fourth time — two new tables in
// one bead is where the copy-paste stopped paying for itself.
func ensureTableBySentinelName(ctx context.Context, db sqlExecQuerier, apply bool, tableName, ddl string) (bool, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tableName).Scan(&name)
	if err == nil {
		return false, nil // already present
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if apply {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return false, err
		}
		return false, nil // applied: no longer outstanding
	}
	return true, nil
}

// ensureLocksModeAndPK brings a pre-loto-k5el.2 DB up to the composite-PK +
// mode-column shape. SQLite cannot ALTER a primary key in place, so when the PK
// is still the legacy single column the locks table is rebuilt (12-step idiom)
// inside the migrate tx, defaulting every existing row's mode to 'exclusive'
// (preserving the pre-mode binary-lock = sole-writer semantics). user_version is
// intentionally NOT bumped — a bump trips MoveCorruptAside and destroys live
// locks (loto-kwlp precedent). Guarded by a PK-shape probe so this is a no-op on
// fresh DBs (CREATE TABLE already declared the composite PK) and on every re-Open.
func ensureLocksModeAndPK(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var pkCols int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE pk > 0`).Scan(&pkCols); err != nil {
		return false, err
	}
	if pkCols == 2 {
		return false, nil // already migrated (fresh DB or prior upgrade)
	}
	// Legacy single-column PK: rebuild. The old table has no `mode` column, so
	// the SELECT supplies the literal 'exclusive' for it. proc_start is present
	// (ensureLocksProcStart ran first), so the column list is valid.
	const rebuild = `
CREATE TABLE locks_new (
  target_canonical TEXT NOT NULL,
  owner_uuid       TEXT NOT NULL,
  session_uuid     TEXT NOT NULL,
  intent           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER NOT NULL,
  host             TEXT NOT NULL,
  pid              INTEGER NOT NULL,
  proc_start       INTEGER,
  branch           TEXT NOT NULL DEFAULT '',
  mode             TEXT NOT NULL DEFAULT 'exclusive',
  PRIMARY KEY (target_canonical, owner_uuid)
);
INSERT INTO locks_new
  (target_canonical, owner_uuid, session_uuid, intent, created_at,
   expires_at, host, pid, proc_start, branch, mode)
SELECT target_canonical, owner_uuid, session_uuid, intent, created_at,
       expires_at, host, pid, proc_start, branch, 'exclusive'
FROM locks;
DROP TABLE locks;
ALTER TABLE locks_new RENAME TO locks;
-- target-only lookups ride the composite PK's leftmost column; no separate index.
CREATE INDEX IF NOT EXISTS idx_locks_owner    ON locks(owner_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_session  ON locks(session_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_expires  ON locks(expires_at);`
	if apply {
		if _, err := db.ExecContext(ctx, rebuild); err != nil {
			return false, err
		}
		return false, nil // applied: no longer outstanding
	}
	return true, nil
}

// ensureEventsCheckCurrent widens the events CHECK constraint to admit the
// kinds added after a DB was created — lock_downgraded (loto-k5el.2), then
// lock_refreshed (ccp-z1vj.6), then the admission verdict pair (loto-ovno.9). A CHECK can't be ALTERed, so the events table is
// rebuilt — but only when the stored DDL lacks the NEWEST kind (probe via
// sqlite_master.sql substring), making this a no-op on fresh DBs and re-Opens.
// The probe tracks the newest kind, so widening the constraint again means
// updating both the probe string and the rebuild DDL below. The probe doubles as
// the events-table existence check for schemaCurrent (ErrNoRows → pending).
// user_version not bumped.
func ensureEventsCheckCurrent(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var ddl string
	if err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='events'`).Scan(&ddl); err != nil {
		return false, err
	}
	if strings.Contains(ddl, "'candidate_rejected'") {
		return false, nil // already current
	}
	const rebuild = `
CREATE TABLE events_new (
  id               TEXT PRIMARY KEY,
  target_canonical TEXT NOT NULL,
  event_kind       TEXT NOT NULL CHECK (event_kind IN ('lock_acquired','lock_released','lock_broken','lock_reclaimed_stale','mode_restore_failed','acquire_rollback_started','lock_downgraded','lock_refreshed','gate_bypass','candidate_accepted','candidate_rejected')),
  actor_uuid       TEXT NOT NULL,
  subject_uuid     TEXT,
  reason           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL
);
INSERT INTO events_new (id, target_canonical, event_kind, actor_uuid, subject_uuid, reason, created_at)
SELECT id, target_canonical, event_kind, actor_uuid, subject_uuid, reason, created_at FROM events;
DROP TABLE events;
ALTER TABLE events_new RENAME TO events;
CREATE INDEX IF NOT EXISTS idx_events_target     ON events(target_canonical, created_at);
CREATE INDEX IF NOT EXISTS idx_events_kind       ON events(event_kind, created_at);
CREATE INDEX IF NOT EXISTS idx_events_created_id ON events(created_at, id);`
	if apply {
		if _, err := db.ExecContext(ctx, rebuild); err != nil {
			return false, err
		}
		return false, nil // applied: no longer outstanding
	}
	return true, nil
}
