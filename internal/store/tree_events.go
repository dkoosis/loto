package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"loto/internal/domain"
)

// treeEventsDDL is the drift layer the tree hooks write on top of the call
// records: what a path was last observed to be, one row per observed change,
// the peer calls that span each change, and the reports those changes owe
// their addressees (enforcement-design.md §3 "observed", "event", "report",
// §5 I3 step 2 and I4 steps 5-6's report half).
//
// Duplicated from schema.sql per the hookCallsDDL precedent: a pending
// ensureFn must be able to apply itself. No user_version bump.
//
// ‡ All four tables are created by ONE ensure step in one transaction, so
// probing for tree_events alone is a sound probe for all four.
const treeEventsDDL = `
CREATE TABLE IF NOT EXISTS path_observed (
  path_canonical TEXT NOT NULL,
  epoch          INTEGER NOT NULL,
  stat           TEXT NOT NULL DEFAULT '',
  digest         TEXT NOT NULL DEFAULT '',
  observed_at    INTEGER NOT NULL,
  PRIMARY KEY (path_canonical, epoch)
);

CREATE TABLE IF NOT EXISTS tree_events (
  event_id       TEXT PRIMARY KEY,
  path_canonical TEXT NOT NULL,
  epoch_pre      INTEGER NOT NULL DEFAULT 0,
  holder_pre     TEXT NOT NULL DEFAULT '',
  holder_now     TEXT NOT NULL DEFAULT '',
  epoch_now      INTEGER NOT NULL DEFAULT 0,
  observer_uuid  TEXT NOT NULL DEFAULT '',
  call_id        TEXT NOT NULL DEFAULT '',
  seq            INTEGER NOT NULL,
  digest_pre     TEXT NOT NULL DEFAULT '',
  digest_post    TEXT NOT NULL DEFAULT '',
  declared       INTEGER NOT NULL DEFAULT 0,
  rule           TEXT NOT NULL DEFAULT '',
  note           TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tree_events_path ON tree_events(path_canonical, seq);

CREATE TABLE IF NOT EXISTS tree_event_spanners (
  event_id   TEXT NOT NULL,
  call_id    TEXT NOT NULL,
  owner_uuid TEXT NOT NULL,
  PRIMARY KEY (event_id, call_id)
);

CREATE TABLE IF NOT EXISTS tree_reports (
  report_id      TEXT PRIMARY KEY,
  event_id       TEXT NOT NULL,
  addressee_uuid TEXT NOT NULL,
  created_at     INTEGER NOT NULL,
  delivered_at   INTEGER,
  acted_at       INTEGER
);
CREATE INDEX IF NOT EXISTS idx_tree_reports_undelivered ON tree_reports(delivered_at, addressee_uuid);`

// ensureTreeEventsTables creates the drift layer in an existing DB that
// predates it. Same shape as ensureHookCallsTables, and pending on the same
// kind of sentinel probe.
func ensureTreeEventsTables(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	return ensureTableBySentinelName(ctx, db, apply, "tree_events", treeEventsDDL)
}

// Verdict-table rule names (enforcement-design.md §5). A report carries the
// rule that produced it so `loto stats` can count reports per rule without
// re-deriving the classification, and so a reader can look the wording up.
//
// ‡ Only the REPORT half of the table is implemented by this layer. `good`,
// provenance, copies and `L`'s row-8 compare-and-set are deferred until §10b's
// acted/reported ratio says whether holders act on reports at all, so rows 2'
// and 8 produce an event and no report.
const (
	TreeRuleDrift = "drift"
	TreeRuleRow1  = "row1"
	TreeRuleRow2  = "row2"
	TreeRuleRow3  = "row3"
	TreeRuleRow4  = "row4"
	TreeRuleRow5  = "row5"
	TreeRuleRow6  = "row6"
	TreeRuleRow7  = "row7"
	TreeRuleRow8  = "row8"
)

// treeRuleNote is the report's wording, one line per rule, taken from §5's
// report column.
//
// ‡ None of these says "X wrote f", and none can. A before/after observation
// over a shared tree says the state changed inside an interval; it never says
// who changed it (§5, "The rule under all of them"). The observer's call and
// the spanning calls are named in the report's own fields, so the wording does
// not have to interpolate them — and cannot accidentally promote one of them
// to author.
var treeRuleNote = map[string]string{ //nolint:gochecknoglobals // read-only wording table
	TreeRuleDrift: "changed with no call covering it",
	TreeRuleRow1:  "lock changed hands during the call",
	TreeRuleRow2:  "changed in your call, no competing call seen",
	TreeRuleRow3:  "your edit overlapped another call; verify the file",
	TreeRuleRow4:  "changed during your call and another call; loto cannot say who",
	TreeRuleRow5:  "changed during the observer's call, no other call seen",
	TreeRuleRow6:  "changed during the observer's call and others; loto cannot say who",
	TreeRuleRow7:  "changed under contention; unlocked, unattributed",
}

// TreeActedWindow is how long after a report a holder's rewrite still counts
// as acting on it (§10b row 2, "within 10 min").
//
// ‡ A constant, not LOTO_T_REPORT. T_report is when an unposted call starts
// being *described* differently; this is the window of a telemetry question
// whose threshold (acted/reported < 1/10, or ≥ 1/2) was written down against
// ten minutes. Letting an env var move it would make the two numbers §10b
// compares incomparable between machines.
const TreeActedWindow = 10 * time.Minute

// treeEventsRetentionMax bounds the drift tables the way eventsRetentionMax
// bounds the audit trail — but only over events nothing is still waiting on.
// treeReportUndeliveredAge is the separate, time-based bound on the ones that
// are: an undelivered report survives any number of later events and expires
// only by age, matching the audit trail's 7 days. See DropOldTreeEvents.
const (
	treeEventsRetentionMax   = 1000
	treeReportUndeliveredAge = eventsRetentionAge
)

// TreeEvent is one observed change of one path: §3's
// `(f, E_pre, h_pre, observer s, call, n: d0 -> d1, declared?)`, plus the rule
// its report was written under. `copy` is absent — this layer stores no bytes.
type TreeEvent struct {
	EventID    string
	Path       string
	EpochPre   int64
	HolderPre  domain.AgentUUID
	EpochNow   int64
	HolderNow  domain.AgentUUID
	Observer   domain.AgentUUID
	CallID     string
	Seq        int64
	DigestPre  string
	DigestPost string
	Declared   bool
	Rule       string
	Note       string
	CreatedAt  time.Time
	// Spanners is every OTHER owner's call that spans this transition, sorted.
	Spanners []TreeSpanner
}

// TreeSpanner is one call that spans a transition: `seq_pre < n` and
// (`n <= seq_post` or the call has not posted).
type TreeSpanner struct {
	CallID string
	Owner  domain.AgentUUID
}

// Contested reports §3's contested predicate: some owner other than the
// observer has a call spanning this transition.
func (e TreeEvent) Contested() bool { return len(e.Spanners) > 0 }

// TreeReport is one verdict's output to one named owner (§3 "report").
// Delivered at the addressee's next pre-hook, and visible in `loto status`
// until then.
type TreeReport struct {
	ReportID  string
	Addressee domain.AgentUUID
	CreatedAt time.Time
	Delivered time.Time
	Event     TreeEvent
}

// treeEventInput is what a caller knows about one change at the moment it
// files the event. Everything else — the event id, the spanners, the rule, the
// reports — is derived here, so no caller can invent a second classification.
type treeEventInput struct {
	Path       string
	EpochPre   int64
	HolderPre  domain.AgentUUID
	EpochNow   int64
	HolderNow  domain.AgentUUID
	Observer   domain.AgentUUID
	CallID     string
	Seq        int64
	DigestPre  string
	DigestPost string
	Declared   bool
	// Drift: filed by I3 step 2, not by a call's post. There is no observer
	// call, so the classification skips the verdict table entirely.
	Drift bool
}

// spannersTx is §3's contested test, and the only place it is written.
//
// ‡ By seq interval, never by digest equality. A call spanning `d0->d1->d2` is
// exonerated from `d1->d2` by a digest test — its pre shows `d0` — which is
// how round 9's design lost causality across two transitions in one call.
// `seq_pre < n` and (`n <= seq_post` or unposted) has no such hole.
//
// A call that has not posted is in flight: t_post NULL AND dead_at NULL. A
// post_missing call is in flight by that test and therefore still spans, which
// is R7's whole point — a command that has not posted may still be running and
// may write next. A call whose owner the probe found dead has ENDED (§3's
// second ending) and spans nothing: §6's "A is dead. Not in flight, so nothing
// spans."
func spannersTx(ctx context.Context, tx *sql.Tx, path string, epoch, seq int64, observer domain.AgentUUID) ([]TreeSpanner, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT p.call_id, c.owner_uuid
  FROM hook_call_paths p JOIN hook_calls c ON c.call_id = p.call_id
 WHERE p.path_canonical = ? AND p.epoch_pre = ?
   AND c.owner_uuid <> ?
   AND p.seq_pre < ?
   AND (p.seq_post >= ? OR (c.t_post IS NULL AND c.dead_at IS NULL))
 ORDER BY c.owner_uuid, p.call_id`, path, epoch, string(observer), seq, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TreeSpanner
	for rows.Next() {
		var callID, owner string
		if err := rows.Scan(&callID, &owner); err != nil {
			return nil, err
		}
		out = append(out, TreeSpanner{CallID: callID, Owner: domain.AgentUUID(owner)})
	}
	return out, rows.Err()
}

// classifyTreeEvent picks the verdict-table rule and the report's addressees.
//
// Rows are tried top to bottom, first match wins, exactly as §5 states them.
// The two rows that report nothing are the two whose only output is a column
// this bead does not build: row 2 declared advances `good` silently, and row 8
// uncontested takes `L` by compare-and-set. Both still file the event, because
// the event is what a later bead re-judges.
//
// Addressees are the holder at pre, the observer, and every spanner — §5's
// report column for rows 3 through 7, with the spanners added on rows 3 and 4
// because a peer told to verify a file it may have written is the whole point
// of naming spanners at all.
func classifyTreeEvent(in treeEventInput, spanners []TreeSpanner) (rule string, addressees []domain.AgentUUID) {
	contested := len(spanners) > 0
	switch {
	case in.Drift:
		rule = TreeRuleDrift
	case in.EpochNow != in.EpochPre:
		rule = TreeRuleRow1
	case in.HolderPre == in.Observer && in.HolderPre != "":
		switch {
		case !contested:
			rule = TreeRuleRow2
		case in.Declared:
			rule = TreeRuleRow3
		default:
			rule = TreeRuleRow4
		}
	case in.HolderPre != "":
		rule = TreeRuleRow5
		if contested {
			rule = TreeRuleRow6
		}
	case contested:
		rule = TreeRuleRow7
	default:
		rule = TreeRuleRow8
	}
	if rule == TreeRuleRow8 || (rule == TreeRuleRow2 && in.Declared) {
		return rule, nil
	}
	return rule, treeAddressees(in, spanners)
}

// treeAddressees is the deduplicated set a report goes to, sorted so the same
// event always addresses the same owners in the same order.
func treeAddressees(in treeEventInput, spanners []TreeSpanner) []domain.AgentUUID {
	seen := map[domain.AgentUUID]bool{}
	var out []domain.AgentUUID
	add := func(u domain.AgentUUID) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	add(in.HolderPre)
	add(in.Observer)
	// Row 1 tells the current holder too: the lock moved to someone who was
	// not in the picture when the call recorded the path.
	if in.EpochNow != in.EpochPre {
		add(in.HolderNow)
	}
	for i := range spanners {
		add(spanners[i].Owner)
	}
	slices.Sort(out)
	return out
}

// fileTreeEventTx files one event, its spanners and its reports in the
// caller's transaction, and appends one tree_change_reported row per report
// (§10b row 2's numerator's denominator). Returns the event as filed.
//
// It runs inside the caller's tx on purpose: the transition number, the event
// that cites it and the report that carries it commit together or not at all,
// so no reader can see a seq with no event or an event with no report.
func fileTreeEventTx(ctx context.Context, tx *sql.Tx, in treeEventInput, now time.Time) (TreeEvent, error) {
	spanners, err := spannersTx(ctx, tx, in.Path, in.EpochPre, in.Seq, in.Observer)
	if err != nil {
		return TreeEvent{}, err
	}
	rule, addressees := classifyTreeEvent(in, spanners)
	ev := TreeEvent{
		EventID: newEventID(), Path: in.Path,
		EpochPre: in.EpochPre, HolderPre: in.HolderPre,
		EpochNow: in.EpochNow, HolderNow: in.HolderNow,
		Observer: in.Observer, CallID: in.CallID, Seq: in.Seq,
		DigestPre: in.DigestPre, DigestPost: in.DigestPost, Declared: in.Declared,
		Rule: rule, Note: treeRuleNote[rule], CreatedAt: now, Spanners: spanners,
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tree_events(event_id, path_canonical, epoch_pre, holder_pre, holder_now, epoch_now,
                        observer_uuid, call_id, seq, digest_pre, digest_post, declared,
                        rule, note, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.EventID, ev.Path, ev.EpochPre, string(ev.HolderPre), string(ev.HolderNow), ev.EpochNow,
		string(ev.Observer), ev.CallID, ev.Seq, ev.DigestPre, ev.DigestPost, boolInt(ev.Declared),
		ev.Rule, ev.Note, now.UnixNano()); err != nil {
		return TreeEvent{}, err
	}
	for i := range spanners {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO tree_event_spanners(event_id, call_id, owner_uuid) VALUES (?,?,?)`,
			ev.EventID, spanners[i].CallID, string(spanners[i].Owner)); err != nil {
			return TreeEvent{}, err
		}
	}
	for _, who := range addressees {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tree_reports(report_id, event_id, addressee_uuid, created_at, delivered_at)
			 VALUES (?,?,?,?,NULL)`,
			newEventID(), ev.EventID, string(who), now.UnixNano()); err != nil {
			return TreeEvent{}, err
		}
		if err := appendTreeChangeReportedTx(ctx, tx, ev, who, now); err != nil {
			return TreeEvent{}, err
		}
	}
	return ev, nil
}

// treeChangeDetail is the tree_change_reported payload: §10b row 2's
// `(f, holder, spanners, row)`, plus the observer's call so a reader can join
// back to the call record while it is still retained.
type treeChangeDetail struct {
	Path     string   `json:"path"`
	Holder   string   `json:"holder"`
	Observer string   `json:"observer,omitempty"`
	CallID   string   `json:"call_id,omitempty"`
	Spanners []string `json:"spanners,omitempty"`
	Rule     string   `json:"rule"`
	Seq      int64    `json:"seq"`
}

// appendTreeChangeReportedTx writes one audit row per REPORT, not per event:
// the number §10b divides into is "how many reports were delivered to a
// holder", and an event addressed to three owners is three chances to act.
func appendTreeChangeReportedTx(ctx context.Context, tx *sql.Tx, ev TreeEvent, who domain.AgentUUID, now time.Time) error {
	d := treeChangeDetail{
		Path: ev.Path, Holder: string(ev.HolderPre), Observer: string(ev.Observer),
		CallID: ev.CallID, Rule: ev.Rule, Seq: ev.Seq,
	}
	for i := range ev.Spanners {
		d.Spanners = append(d.Spanners, string(ev.Spanners[i].Owner))
	}
	payload, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return appendEventTx(ctx, tx, domain.Event{
		Kind:      EventTreeChangeReported,
		Target:    domain.Target{Canonical: ev.Path},
		ActorUUID: string(who),
		Reason:    ev.Rule,
		Detail:    string(payload),
		CreatedAt: now,
	})
}

// markTreeChangeActedTx writes §10b row 2's numerator: this owner was HANDED a
// report saying `f` went `d0 -> d1`, and inside TreeActedWindow of that
// delivery its own call put `d0` back. Returns the paths it counted.
//
// ‡ Three constraints, each one a way the ratio would otherwise be wrong.
// (1) The window runs from delivered_at, not created_at: a report nobody has
// read cannot have moved anyone, so a rewrite before delivery is a
// coincidence, and an undelivered report is never acted on at all.
// (2) A report is counted AT MOST ONCE — acted_at is stamped on the row in the
// same statement that selects it — so a session that keeps working while the
// file stays reverted does not add an acted row per tool call.
// (3) "Acted" is deliberately weaker than "restored": `loto restore` does not
// exist yet, and the counter has to exist BEFORE it to answer whether it is
// worth building, so the observable is the one thing a holder can do today.
// Digest equality is sound here and nowhere else — this is not an attribution
// question, it is "is the content the report named back on disk".
func markTreeChangeActedTx(ctx context.Context, tx *sql.Tx, owner domain.AgentUUID, path, digest string, now time.Time) ([]string, error) {
	if digest == "" || owner == "" {
		return nil, nil
	}
	answered, err := answeredReportsTx(ctx, tx, owner, path, digest, now)
	if err != nil || len(answered) == 0 {
		return nil, err
	}
	acted := make([]domain.Event, 0, len(answered))
	for _, r := range answered {
		res, err := tx.ExecContext(ctx,
			`UPDATE tree_reports SET acted_at = ? WHERE report_id = ? AND acted_at IS NULL`,
			now.UnixNano(), r.reportID)
		if err != nil {
			return nil, err
		}
		// A concurrent post for the same owner may have claimed it first;
		// whoever wins the UPDATE writes the one audit row.
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n == 0 {
			continue
		}
		payload, err := json.Marshal(treeChangeDetail{
			Path: path, Holder: r.holder, Rule: r.rule, Seq: r.seq,
		})
		if err != nil {
			return nil, err
		}
		acted = append(acted, domain.Event{
			Kind:      EventTreeChangeActed,
			Target:    domain.Target{Canonical: path},
			ActorUUID: string(owner),
			Reason:    r.rule,
			Detail:    string(payload),
			CreatedAt: now,
		})
	}
	if len(acted) == 0 {
		return nil, nil
	}
	return []string{path}, appendEventsTx(ctx, tx, acted)
}

// answeredReport is one delivered, not-yet-acted report whose pre-digest is
// what the path now holds.
type answeredReport struct {
	reportID string
	rule     string
	holder   string
	seq      int64
}

// answeredReportsTx reads them, so the UPDATE loop above has its own scope and
// the rows handle is closed before any write on the same tx.
func answeredReportsTx(ctx context.Context, tx *sql.Tx, owner domain.AgentUUID, path, digest string, now time.Time) ([]answeredReport, error) {
	rows, err := tx.QueryContext(ctx, answeredReportsSQL,
		string(owner), path, digest, now.Add(-TreeActedWindow).UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []answeredReport
	for rows.Next() {
		var r answeredReport
		if err := rows.Scan(&r.reportID, &r.rule, &r.seq, &r.holder); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// answeredReportsSQL finds every report this owner was HANDED inside the
// window that said the path went from the digest it is now back at, and that
// nothing has been counted against yet.
const answeredReportsSQL = `
SELECT r.report_id, e.rule, e.seq, e.holder_pre
  FROM tree_reports r JOIN tree_events e ON e.event_id = r.event_id
 WHERE r.addressee_uuid = ? AND e.path_canonical = ? AND e.digest_pre = ?
   AND r.delivered_at IS NOT NULL AND r.delivered_at >= ? AND r.acted_at IS NULL
 ORDER BY e.seq DESC, r.report_id`

// DriftOutcome is what one drift pass found. Each path appears in at most one
// slice, and a path whose state matches what was last observed appears in none.
type DriftOutcome struct {
	// Drifted is every locked path whose state moved with no call covering it.
	Drifted []string
	// Seeded is every locked path this pass observed for the FIRST time —
	// never a drift, because nothing was known to differ from.
	Seeded []string
	// Deferred is every changed path some in-flight call already recorded:
	// its post will file the event, so filing one here too would double-count.
	Deferred []string
}

// RecordDrift is I3 step 2. For each locked path whose stat or digest differs
// from `observed(f)`, a change happened that no call's window covered — a
// background process, a human, a post that never ran. It takes the next
// `seq(f)`, files an event, reports to the holder, and sets `observed(f)` to
// the current state so the same drift is never reported twice.
//
// obs is the pre-hook's observation, and the SAME observation the call record
// is about to be written from. That is what makes §10a test 11 hold: step 2
// sets observed to the digest step 4 is about to record, so the post compares
// the new digest against itself and files nothing.
//
// A locked path with no `observed` row is SEEDED, not reported. `observed` is
// keyed (f, E), so the first pre after a lock is taken — or after the lock
// changes hands — has nothing to compare against, and inventing a drift there
// would report every lock acquisition as a mutation.
func (s *Store) RecordDrift(ctx context.Context, observer domain.AgentUUID, now time.Time, obs []HookPathState) (DriftOutcome, error) {
	var out DriftOutcome
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return out, err
	}
	defer cleanup()
	wrote := false
	for i := range obs {
		if !obs[i].Locked {
			continue
		}
		verdict, err := driftOnePathTx(ctx, tx, observer, obs[i], now)
		if err != nil {
			return DriftOutcome{}, err
		}
		switch verdict {
		case driftFiled:
			out.Drifted = append(out.Drifted, obs[i].Path)
			wrote = true
		case driftSeeded:
			out.Seeded = append(out.Seeded, obs[i].Path)
			wrote = true
		case driftDeferred:
			out.Deferred = append(out.Deferred, obs[i].Path)
		case driftUnchanged:
		}
	}
	if !wrote {
		return out, nil
	}
	if err := commitTxFn(tx); err != nil {
		return DriftOutcome{}, err
	}
	sort.Strings(out.Drifted)
	sort.Strings(out.Seeded)
	sort.Strings(out.Deferred)
	return out, nil
}

// driftVerdict is what one locked path's drift pass decided.
type driftVerdict int

const (
	driftUnchanged driftVerdict = iota
	driftSeeded
	driftFiled
	driftDeferred
)

// driftOnePathTx handles one locked path.
//
// ‡ A change some in-flight call already recorded the path for is DEFERRED,
// not filed. Drift means "no call's window covered this"; a call that recorded
// f and has not posted covers it by construction, and its post will file the
// event with the spanning calls named. Filing here as well would give one
// physical change two events — a drift whose own note says "no call covering
// it" while naming that call as a spanner, plus the peer's — and two report
// sets, which is a straight skew of §10b's reported count.
//
// Deferring writes NOTHING, `observed(f)` included, so the change is not lost:
// if the peer posts, its post moves observed and the next pre sees nothing; if
// its owner dies, the dead-owner sweep ends the call and the next pre files the
// drift then. The observer's own in-flight calls count too — the property that
// matters is that some post is still coming, not whose.
func driftOnePathTx(ctx context.Context, tx *sql.Tx, observer domain.AgentUUID, o HookPathState, now time.Time) (driftVerdict, error) {
	prevStat, prevDigest, known, err := observedAtTx(ctx, tx, o.Path, o.Epoch)
	if err != nil {
		return driftUnchanged, err
	}
	if known && prevStat == o.Stat && prevDigest == o.Digest {
		return driftUnchanged, nil
	}
	if !known {
		return driftSeeded, setObservedTx(ctx, tx, o, now)
	}
	covered, err := inFlightCallRecordedTx(ctx, tx, o.Path, o.Epoch)
	if err != nil {
		return driftUnchanged, err
	}
	if covered {
		return driftDeferred, nil
	}
	seq, err := nextPathSeq(ctx, tx, o.Path, o.Epoch, o.Digest)
	if err != nil {
		return driftUnchanged, err
	}
	if _, err := fileTreeEventTx(ctx, tx, treeEventInput{
		Path: o.Path, EpochPre: o.Epoch, HolderPre: o.Holder,
		EpochNow: o.Epoch, HolderNow: o.Holder,
		Observer: observer, Seq: seq,
		DigestPre: prevDigest, DigestPost: o.Digest, Drift: true,
	}, now); err != nil {
		return driftUnchanged, err
	}
	return driftFiled, setObservedTx(ctx, tx, o, now)
}

// inFlightCallRecordedTx reports whether some open call recorded this path at
// this epoch — the call whose post will file the change drift would otherwise
// file a second time.
func inFlightCallRecordedTx(ctx context.Context, tx *sql.Tx, path string, epoch int64) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM hook_call_paths p JOIN hook_calls c ON c.call_id = p.call_id
 WHERE p.path_canonical = ? AND p.epoch_pre = ? AND c.t_post IS NULL AND c.dead_at IS NULL`,
		path, epoch).Scan(&n)
	return n > 0, err
}

func observedAtTx(ctx context.Context, tx *sql.Tx, path string, epoch int64) (stat, digest string, known bool, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT stat, digest FROM path_observed WHERE path_canonical = ? AND epoch = ?`,
		path, epoch).Scan(&stat, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return stat, digest, true, nil
}

func setObservedTx(ctx context.Context, tx *sql.Tx, o HookPathState, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO path_observed(path_canonical, epoch, stat, digest, observed_at) VALUES (?,?,?,?,?)
ON CONFLICT(path_canonical, epoch) DO UPDATE SET stat = excluded.stat,
                                                 digest = excluded.digest,
                                                 observed_at = excluded.observed_at`,
		o.Path, o.Epoch, o.Stat, o.Digest, now.UnixNano())
	return err
}

// UndeliveredReports lists every report nobody has been handed yet, oldest
// first. addressee empty means every owner's — `loto status` answers "what is
// waiting on this ground", including a report addressed to a session that
// died before its next pre-hook (§6, "the report waits in loto status").
func (s *Store) UndeliveredReports(ctx context.Context, addressee domain.AgentUUID) ([]TreeReport, error) {
	if addressee == "" {
		return s.readReports(ctx, undeliveredAllSQL)
	}
	return s.readReports(ctx, undeliveredForOneSQL, string(addressee))
}

// DeliverReports hands every undelivered report addressed to this owner over
// and marks it delivered, in one transaction: a report read but not marked
// would be delivered again at the next call, and a report marked but not read
// would be lost (§3, "delivered at each addressee's next pre-hook").
func (s *Store) DeliverReports(ctx context.Context, addressee domain.AgentUUID, now time.Time) ([]TreeReport, error) {
	if addressee == "" {
		return nil, nil
	}
	pending, err := s.UndeliveredReports(ctx, addressee)
	if err != nil || len(pending) == 0 {
		return nil, err
	}
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	delivered := make([]TreeReport, 0, len(pending))
	for i := range pending {
		res, err := tx.ExecContext(ctx,
			`UPDATE tree_reports SET delivered_at = ? WHERE report_id = ? AND delivered_at IS NULL`,
			now.UnixNano(), pending[i].ReportID)
		if err != nil {
			return nil, err
		}
		// A concurrent pre-hook for the same owner may have taken it first.
		// Delivering it twice is the one outcome worth preventing here.
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n == 0 {
			continue
		}
		pending[i].Delivered = now
		delivered = append(delivered, pending[i])
	}
	if err := commitTxFn(tx); err != nil {
		return nil, err
	}
	return delivered, nil
}

const treeReportSelect = `SELECT r.report_id, r.addressee_uuid, r.created_at, r.delivered_at,
       e.event_id, e.path_canonical, e.epoch_pre, e.holder_pre, e.holder_now, e.epoch_now,
       e.observer_uuid, e.call_id, e.seq, e.digest_pre, e.digest_post, e.declared,
       e.rule, e.note, e.created_at
  FROM tree_reports r JOIN tree_events e ON e.event_id = r.event_id
 WHERE r.delivered_at IS NULL`

// treeReportOrder is chronological, then fully tie-broken. report_id is
// random, so ordering by it alone lets one input render two ways — the thing
// .claude/rules/design.md forbids outright ("same input, byte-identical
// output"), and every report of one event shares a created_at.
const treeReportOrder = ` ORDER BY r.created_at, e.path_canonical, e.seq, r.addressee_uuid, r.report_id`

// The two undelivered-report queries, whole and constant. Written out rather
// than assembled from a predicate argument: a query built by string
// concatenation at run time is one refactor away from taking a value instead
// of a literal, and there are exactly two shapes.
const (
	undeliveredAllSQL    = treeReportSelect + treeReportOrder
	undeliveredForOneSQL = treeReportSelect + ` AND r.addressee_uuid = ?` + treeReportOrder
)

// readReports runs one of the queries above and hangs each event's spanners
// off the result.
func (s *Store) readReports(ctx context.Context, query string, args ...any) ([]TreeReport, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TreeReport
	for rows.Next() {
		r, err := scanTreeReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		sp, err := s.spannersOf(ctx, out[i].Event.EventID)
		if err != nil {
			return nil, err
		}
		out[i].Event.Spanners = sp
	}
	return out, nil
}

func scanTreeReport(rows *sql.Rows) (TreeReport, error) {
	var (
		r                                 TreeReport
		addressee                         string
		createdNs, evCreatedNs            int64
		deliveredNs                       sql.NullInt64
		holderPre, holderNow, observerStr string
		declared                          int
	)
	if err := rows.Scan(&r.ReportID, &addressee, &createdNs, &deliveredNs,
		&r.Event.EventID, &r.Event.Path, &r.Event.EpochPre, &holderPre, &holderNow, &r.Event.EpochNow,
		&observerStr, &r.Event.CallID, &r.Event.Seq, &r.Event.DigestPre, &r.Event.DigestPost, &declared,
		&r.Event.Rule, &r.Event.Note, &evCreatedNs); err != nil {
		return TreeReport{}, err
	}
	r.Addressee = domain.AgentUUID(addressee)
	r.CreatedAt = time.Unix(0, createdNs)
	if deliveredNs.Valid {
		r.Delivered = time.Unix(0, deliveredNs.Int64)
	}
	r.Event.HolderPre = domain.AgentUUID(holderPre)
	r.Event.HolderNow = domain.AgentUUID(holderNow)
	r.Event.Observer = domain.AgentUUID(observerStr)
	r.Event.Declared = declared != 0
	r.Event.CreatedAt = time.Unix(0, evCreatedNs)
	return r, nil
}

func (s *Store) spannersOf(ctx context.Context, eventID string) ([]TreeSpanner, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT call_id, owner_uuid FROM tree_event_spanners WHERE event_id = ? ORDER BY owner_uuid, call_id`,
		eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TreeSpanner
	for rows.Next() {
		var callID, owner string
		if err := rows.Scan(&callID, &owner); err != nil {
			return nil, err
		}
		out = append(out, TreeSpanner{CallID: callID, Owner: domain.AgentUUID(owner)})
	}
	return out, rows.Err()
}

// TreeEventsFor reads every event filed against one path, oldest transition
// first, with its spanners. The read `loto stats` and the tests use; nothing
// in the hook path calls it.
func (s *Store) TreeEventsFor(ctx context.Context, path string) ([]TreeEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT event_id, path_canonical, epoch_pre, holder_pre, holder_now, epoch_now,
       observer_uuid, call_id, seq, digest_pre, digest_post, declared, rule, note, created_at
  FROM tree_events WHERE path_canonical = ? ORDER BY seq, event_id`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TreeEvent
	for rows.Next() {
		var (
			e                                 TreeEvent
			holderPre, holderNow, observerStr string
			declared                          int
			createdNs                         int64
		)
		if err := rows.Scan(&e.EventID, &e.Path, &e.EpochPre, &holderPre, &holderNow, &e.EpochNow,
			&observerStr, &e.CallID, &e.Seq, &e.DigestPre, &e.DigestPost, &declared,
			&e.Rule, &e.Note, &createdNs); err != nil {
			return nil, err
		}
		e.HolderPre = domain.AgentUUID(holderPre)
		e.HolderNow = domain.AgentUUID(holderNow)
		e.Observer = domain.AgentUUID(observerStr)
		e.Declared = declared != 0
		e.CreatedAt = time.Unix(0, createdNs)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		sp, err := s.spannersOf(ctx, out[i].EventID)
		if err != nil {
			return nil, err
		}
		out[i].Spanners = sp
	}
	return out, nil
}

// DropOldTreeEvents bounds the drift tables, in two passes with two different
// rules, and returns how many events it dropped.
//
// ‡ An UNDELIVERED report is never evicted by the row cap. Three busy sessions
// file a thousand events in under an hour, so a row cap alone silently deletes
// the reports waiting for an idle session — while `tree_change_reported` still
// counts them, which skews the one ratio this whole stage exists to measure,
// in the direction that says holders ignore reports. Their event is exempt
// from the cap and bounded by AGE instead (the same 7 days the audit trail
// uses), and an aged-out undelivered report leaves a `tree_report_dropped`
// row, so the loss is countable rather than invisible.
func (s *Store) DropOldTreeEvents(ctx context.Context, now time.Time) (int, error) {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	if err := dropAgedUndeliveredTx(ctx, tx, now); err != nil {
		return 0, err
	}
	// The cap, over events nothing is still waiting on. An event with an
	// undelivered report is not a candidate at any depth.
	res, err := tx.ExecContext(ctx, `
DELETE FROM tree_events WHERE event_id IN (
  SELECT event_id FROM tree_events
   WHERE event_id NOT IN (SELECT event_id FROM tree_reports WHERE delivered_at IS NULL)
   ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET ?
)`, treeEventsRetentionMax)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		for _, stmt := range []string{
			`DELETE FROM tree_event_spanners WHERE event_id NOT IN (SELECT event_id FROM tree_events)`,
			`DELETE FROM tree_reports WHERE event_id NOT IN (SELECT event_id FROM tree_events)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return 0, err
			}
		}
	}
	if err := commitTxFn(tx); err != nil {
		return 0, err
	}
	return int(n), nil
}

// dropAgedUndeliveredTx deletes every undelivered report older than
// treeReportUndeliveredAge and leaves one tree_report_dropped row per report,
// so a report that expired unread is a number somebody can read rather than a
// silent hole in the denominator.
func dropAgedUndeliveredTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	ids, dropped, err := agedUndeliveredTx(ctx, tx, now)
	if err != nil || len(ids) == 0 {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tree_reports WHERE report_id = ?`, id); err != nil {
			return err
		}
	}
	return appendEventsTx(ctx, tx, dropped)
}

// agedUndeliveredTx reads them, so the rows handle is closed before any write
// runs on the same transaction.
func agedUndeliveredTx(ctx context.Context, tx *sql.Tx, now time.Time) ([]string, []domain.Event, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT r.report_id, r.addressee_uuid, e.path_canonical, e.rule, e.seq, e.holder_pre
  FROM tree_reports r JOIN tree_events e ON e.event_id = r.event_id
 WHERE r.delivered_at IS NULL AND r.created_at < ?
 ORDER BY r.report_id`, now.Add(-treeReportUndeliveredAge).UnixNano())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var (
		ids     []string
		dropped []domain.Event
	)
	for rows.Next() {
		var reportID, addressee, path, rule, holder string
		var seq int64
		if err := rows.Scan(&reportID, &addressee, &path, &rule, &seq, &holder); err != nil {
			return nil, nil, err
		}
		payload, err := json.Marshal(treeChangeDetail{Path: path, Holder: holder, Rule: rule, Seq: seq})
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, reportID)
		dropped = append(dropped, domain.Event{
			Kind:      EventTreeReportDropped,
			Target:    domain.Target{Canonical: path},
			ActorUUID: addressee,
			Reason:    rule,
			Detail:    string(payload),
			CreatedAt: now,
		})
	}
	return ids, dropped, rows.Err()
}
