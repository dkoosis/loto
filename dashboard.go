package loto

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// EventKind enumerates the dashboard event types streamed by Watch and
// reconstructed from disk by Backfill.
type EventKind string

const (
	EventHeld       EventKind = "held"
	EventReleased   EventKind = "released"
	EventReserved   EventKind = "reserved"
	EventUnreserved EventKind = "unreserved"
	EventMsg        EventKind = "msg"
)

// Event is a single dashboard observation. Target is a file path for
// held/released/msg and a glob pattern for reserved/unreserved.
type Event struct {
	Time   time.Time
	Kind   EventKind
	Agent  string
	Target string
	Intent string
	To     string // msg recipient
	Body   string // msg body
}

// Backfill reconstructs events visible on disk whose timestamp is at or after
// since. Result is sorted by Time. Released/unreserved events cannot be
// recovered from disk and are omitted.
func (l *LOTO) Backfill(since time.Time) ([]Event, error) {
	out := backfillFiles(filepath.Join(l.baseDir, "files"), since)
	out = append(out, backfillReservations(l, since)...)
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

func backfillFiles(filesDir string, since time.Time) []Event {
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		return nil
	}
	var out []Event
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(filesDir, name)
		switch {
		case strings.HasSuffix(name, ".tag"):
			if ev, ok := backfillTag(path, since); ok {
				out = append(out, ev)
			}
		case strings.HasSuffix(name, ".msgs"):
			out = append(out, backfillMsgs(path, since)...)
		}
	}
	return out
}

func backfillTag(path string, since time.Time) (Event, bool) {
	t, err := readTag(path)
	if err != nil || t == nil || t.Timestamp.Before(since) {
		return Event{}, false
	}
	return Event{
		Time:   t.Timestamp,
		Kind:   EventHeld,
		Agent:  t.AgentID,
		Target: t.Target,
		Intent: t.Intent,
	}, true
}

func backfillMsgs(path string, since time.Time) []Event {
	msgs, err := readMsgs(path)
	if err != nil {
		return nil
	}
	out := make([]Event, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		if m.Timestamp.Before(since) {
			continue
		}
		out = append(out, Event{
			Time:   m.Timestamp,
			Kind:   EventMsg,
			Agent:  m.From,
			Target: m.Target,
			To:     m.To,
			Body:   m.Body,
		})
	}
	return out
}

func backfillReservations(l *LOTO, since time.Time) []Event {
	resDir := l.reservationsDir()
	entries, err := os.ReadDir(resDir)
	if err != nil {
		return nil
	}
	var out []Event
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), reservationExt) {
			continue
		}
		r, rerr := l.readReservation(filepath.Join(resDir, e.Name()))
		if rerr != nil || r == nil || r.CreatedAt.Before(since) {
			continue
		}
		out = append(out, Event{
			Time:   r.CreatedAt,
			Kind:   EventReserved,
			Agent:  r.AgentID,
			Target: r.Pattern,
			Intent: r.Intent,
		})
	}
	return out
}

// Watch streams live events from baseDir until ctx is canceled. The returned
// channel is closed when the watcher terminates. Events emitted include
// held/released for file locks, reserved/unreserved for reservations, and
// msg for mailbox writes. Released/unreserved are reconstructed from a
// per-path cache populated on Create/Write events; an event observed before
// the watcher started is reported as held but its release will be reported
// once seen (cache is seeded from existing tags at startup).
func (l *LOTO) Watch(ctx context.Context) (<-chan Event, error) {
	filesDir := filepath.Join(l.baseDir, "files")
	resDir := filepath.Join(l.baseDir, "reservations")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return nil, &ErrSystem{Op: "dashboard: mkdir files", Err: err}
	}
	if err := os.MkdirAll(resDir, 0o700); err != nil {
		return nil, &ErrSystem{Op: "dashboard: mkdir reservations", Err: err}
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, &ErrSystem{Op: "dashboard: new watcher", Err: err}
	}
	if err := w.Add(filesDir); err != nil {
		_ = w.Close()
		return nil, &ErrSystem{Op: "dashboard: watch files", Err: err}
	}
	if err := w.Add(resDir); err != nil {
		_ = w.Close()
		return nil, &ErrSystem{Op: "dashboard: watch reservations", Err: err}
	}

	tagCache := map[string]Tag{}            // tag path → last tag
	resCache := map[string]Reservation{}    // tag path → last reservation
	msgSeen := map[string]map[string]bool{} // msgs path → seen MsgIDs
	// Seed synchronously so the watcher's caches reflect existing state
	// before any caller-driven write can race with the watcher goroutine.
	seedDashboardCaches(l, filesDir, resDir, tagCache, resCache, msgSeen)

	out := make(chan Event, 64)
	go runWatch(ctx, l, w, filesDir, resDir, tagCache, resCache, msgSeen, out)
	return out, nil
}

func runWatch(ctx context.Context, l *LOTO, w *fsnotify.Watcher, filesDir, resDir string,
	tagCache map[string]Tag, resCache map[string]Reservation, msgSeen map[string]map[string]bool,
	out chan<- Event,
) {
	defer close(out)
	defer w.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			emitDashboardEvent(ev, l, filesDir, resDir, tagCache, resCache, msgSeen, out)
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
		}
	}
}

func seedDashboardCaches(l *LOTO, filesDir, resDir string,
	tagCache map[string]Tag, resCache map[string]Reservation, msgSeen map[string]map[string]bool,
) {
	seedFilesDir(filesDir, tagCache, msgSeen)
	seedReservationsDir(l, resDir, resCache)
}

func seedFilesDir(filesDir string, tagCache map[string]Tag, msgSeen map[string]map[string]bool) {
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(filesDir, name)
		switch {
		case strings.HasSuffix(name, ".tag"):
			if t, err := readTag(path); err == nil && t != nil {
				tagCache[path] = *t
			}
		case strings.HasSuffix(name, ".msgs"):
			msgSeen[path] = seedMsgSeen(path)
		}
	}
}

func seedMsgSeen(path string) map[string]bool {
	seen := map[string]bool{}
	msgs, err := readMsgs(path)
	if err != nil {
		return seen
	}
	for i := range msgs {
		m := &msgs[i]
		if m.MsgID != "" {
			seen[m.MsgID] = true
		}
	}
	return seen
}

func seedReservationsDir(l *LOTO, resDir string, resCache map[string]Reservation) {
	entries, err := os.ReadDir(resDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), reservationExt) {
			continue
		}
		path := filepath.Join(resDir, e.Name())
		if r, err := l.readReservation(path); err == nil && r != nil {
			resCache[path] = *r
		}
	}
}

func emitDashboardEvent(ev fsnotify.Event, l *LOTO, filesDir, resDir string,
	tagCache map[string]Tag, resCache map[string]Reservation, msgSeen map[string]map[string]bool,
	out chan<- Event,
) {
	dir := filepath.Dir(ev.Name)
	switch {
	case dir == filesDir && strings.HasSuffix(ev.Name, ".tag"):
		handleFileTagEvent(ev, tagCache, out)
	case dir == filesDir && strings.HasSuffix(ev.Name, ".msgs"):
		handleMsgsEvent(ev, msgSeen, out)
	case dir == resDir && strings.HasSuffix(ev.Name, reservationExt):
		handleResEvent(ev, l, resCache, out)
	}
}

func handleFileTagEvent(ev fsnotify.Event, cache map[string]Tag, out chan<- Event) {
	if ev.Op&(fsnotify.Create|fsnotify.Write) != 0 {
		t := readTagWithRetry(ev.Name)
		if t == nil {
			return
		}
		prev, hadPrev := cache[ev.Name]
		cache[ev.Name] = *t
		// Suppress duplicate emit when Write follows Create with identical tag.
		if hadPrev && prev.AgentID == t.AgentID && prev.Timestamp.Equal(t.Timestamp) {
			return
		}
		out <- Event{
			Time:   t.Timestamp,
			Kind:   EventHeld,
			Agent:  t.AgentID,
			Target: t.Target,
			Intent: t.Intent,
		}
		return
	}
	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		prev, ok := cache[ev.Name]
		if !ok {
			return
		}
		delete(cache, ev.Name)
		out <- Event{
			Time:   time.Now().UTC(),
			Kind:   EventReleased,
			Agent:  prev.AgentID,
			Target: prev.Target,
			Intent: prev.Intent,
		}
	}
}

func handleResEvent(ev fsnotify.Event, l *LOTO, cache map[string]Reservation, out chan<- Event) {
	if ev.Op&(fsnotify.Create|fsnotify.Write) != 0 {
		r := readReservationWithRetry(l, ev.Name)
		if r == nil {
			return
		}
		prev, hadPrev := cache[ev.Name]
		cache[ev.Name] = *r
		if hadPrev && prev.AgentID == r.AgentID && prev.CreatedAt.Equal(r.CreatedAt) {
			return
		}
		out <- Event{
			Time:   r.CreatedAt,
			Kind:   EventReserved,
			Agent:  r.AgentID,
			Target: r.Pattern,
			Intent: r.Intent,
		}
		return
	}
	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		prev, ok := cache[ev.Name]
		if !ok {
			return
		}
		delete(cache, ev.Name)
		out <- Event{
			Time:   time.Now().UTC(),
			Kind:   EventUnreserved,
			Agent:  prev.AgentID,
			Target: prev.Pattern,
			Intent: prev.Intent,
		}
	}
}

// readTagWithRetry handles the macOS fsnotify race where a Create event can
// fire before the file's content is fully flushed. Returns nil after retries.
func readTagWithRetry(path string) *Tag {
	for range 5 {
		if t, err := readTag(path); err == nil && t != nil {
			return t
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func readReservationWithRetry(l *LOTO, path string) *Reservation {
	for range 5 {
		if r, err := l.readReservation(path); err == nil && r != nil {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func handleMsgsEvent(ev fsnotify.Event, msgSeen map[string]map[string]bool, out chan<- Event) {
	if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
		return
	}
	seen, ok := msgSeen[ev.Name]
	if !ok {
		seen = map[string]bool{}
		msgSeen[ev.Name] = seen
	}
	msgs, err := readMsgs(ev.Name)
	if err != nil {
		return
	}
	for i := range msgs {
		m := &msgs[i]
		if m.MsgID != "" && seen[m.MsgID] {
			continue
		}
		if m.MsgID != "" {
			seen[m.MsgID] = true
		}
		out <- Event{
			Time:   m.Timestamp,
			Kind:   EventMsg,
			Agent:  m.From,
			Target: m.Target,
			To:     m.To,
			Body:   m.Body,
		}
	}
}
