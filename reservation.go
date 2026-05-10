// On-disk shape (reservations):
//
//	<baseDir>/reservations/<sha256(pattern)>.tag   JSON Reservation, advisory
//
// One file per reservation. Body is a JSON-encoded Reservation (see struct
// for fields). Mode 0600. No flock — reservations are purely advisory hints
// for tooling/UI; conflicts surface at TryFileLock time, not at Reserve time.
// Two agents may hold reservations whose patterns overlap. Expired tags
// (ExpiresAt past) are pruned lazily on read.

package loto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

const reservationExt = ".tag"

// ErrInvalidGlob is returned when a reservation pattern is not a valid doublestar glob.
var ErrInvalidGlob = errors.New("invalid glob pattern")

// ErrReservationExpired is returned when a reservation file existed but is past its TTL.
var ErrReservationExpired = errors.New("reservation expired")

// Reservation is an advisory glob-pattern hold on a subtree.
// Stored at <baseDir>/reservations/<hash>.tag; no flock (purely advisory).
type Reservation struct {
	AgentID   string     `json:"agent_id"`
	Intent    string     `json:"intent"`
	Pattern   string     `json:"pattern"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Reserve writes an advisory reservation for the given glob pattern.
// Returns the reservation on success. No exclusive lock is taken; two agents
// can reserve overlapping patterns — conflicts surface at TryFileLock time.
func (l *LOTO) Reserve(agentID, intent, pattern string, ttl time.Duration) (*Reservation, error) {
	if !doublestar.ValidatePattern(pattern) {
		return nil, &ErrSystem{Op: "reserve: invalid pattern", Err: fmt.Errorf("%w: %q", ErrInvalidGlob, pattern)}
	}
	resDir := l.reservationsDir()
	if err := os.MkdirAll(resDir, 0o700); err != nil {
		return nil, &ErrSystem{Op: "reserve: mkdir", Err: err}
	}
	r := &Reservation{
		AgentID:   agentID,
		Intent:    intent,
		Pattern:   pattern,
		CreatedAt: time.Now().UTC(),
	}
	if ttl > 0 {
		exp := r.CreatedAt.Add(ttl)
		r.ExpiresAt = &exp
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, &ErrSystem{Op: "reserve: marshal", Err: err}
	}
	tagPath := filepath.Join(resDir, hashPattern(pattern)+reservationExt)
	if err := atomicWriteReservation(tagPath, append(data, '\n')); err != nil {
		return nil, &ErrSystem{Op: "reserve: write", Err: err}
	}
	return r, nil
}

// atomicWriteReservation writes payload to tagPath via a temp-file + rename
// so concurrent readers never observe a partial file (which would otherwise
// be quarantined as corrupt). Bead loto-8ru surfaced this via stress.
func atomicWriteReservation(tagPath string, payload []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(tagPath), filepath.Base(tagPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, werr := tmp.Write(payload); werr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return werr
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(tmpName)
		return cerr
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, tagPath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Unreserve removes the reservation for the given pattern, if it exists.
func (l *LOTO) Unreserve(pattern string) error {
	tagPath := filepath.Join(l.reservationsDir(), hashPattern(pattern)+reservationExt)
	if err := os.Remove(tagPath); err != nil && !os.IsNotExist(err) {
		return &ErrSystem{Op: "unreserve: remove", Err: err}
	}
	return nil
}

// ListReservations returns all active (non-expired) reservations. Corrupt
// reservation files are quarantined to a .corrupt sidecar (with a stderr
// warning) so coordination state never silently drops entries.
func (l *LOTO) ListReservations() ([]*Reservation, error) {
	resDir := l.reservationsDir()
	entries, err := os.ReadDir(resDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &ErrSystem{Op: "list-reservations: readdir", Err: err}
	}
	var out []*Reservation
	for _, e := range entries {
		if filepath.Ext(e.Name()) != reservationExt {
			continue
		}
		tagPath := filepath.Join(resDir, e.Name())
		r, err := l.readReservation(tagPath)
		if err != nil {
			if errors.Is(err, ErrReservationExpired) || os.IsNotExist(err) {
				continue
			}
			quarantineCorruptReservation(tagPath, err)
			continue
		}
		if r == nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// quarantineCorruptReservation renames a malformed reservation tag to a
// .corrupt sidecar so it is visible to operators but no longer participates
// in coordination. Best-effort: errors are logged, never propagated.
func quarantineCorruptReservation(tagPath string, parseErr error) {
	corruptPath := fmt.Sprintf("%s.corrupt-%d", tagPath, time.Now().UnixNano())
	fmt.Fprintf(os.Stderr, "loto: warning: corrupt reservation %s (%v); quarantined to %s\n",
		tagPath, parseErr, corruptPath)
	if err := os.Rename(tagPath, corruptPath); err != nil {
		fmt.Fprintf(os.Stderr, "loto: warning: cannot quarantine %s: %v\n", tagPath, err)
	}
}

// ConflictingReservations returns active reservations whose pattern matches path.
// Called at TryFileLock time to surface advisory conflicts.
func (l *LOTO) ConflictingReservations(path string) ([]*Reservation, error) {
	all, err := l.ListReservations()
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	var conflicts []*Reservation
	for _, r := range all {
		// Match both the raw path and the absolute path against the pattern.
		if matchesReservation(r.Pattern, abs) || matchesReservation(r.Pattern, path) {
			conflicts = append(conflicts, r)
		}
	}
	return conflicts, nil
}

func matchesReservation(pattern, path string) bool {
	ok, _ := doublestar.Match(pattern, path)
	return ok
}

func (l *LOTO) readReservation(tagPath string) (*Reservation, error) {
	data, err := os.ReadFile(tagPath)
	if err != nil {
		return nil, err
	}
	var r Reservation
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	// Drop expired reservations (lazy GC on read).
	if r.ExpiresAt != nil && time.Now().After(*r.ExpiresAt) {
		_ = os.Remove(tagPath)
		return nil, ErrReservationExpired
	}
	return &r, nil
}

func (l *LOTO) reservationsDir() string {
	return filepath.Join(l.baseDir, "reservations")
}

func hashPattern(pattern string) string {
	sum := sha256.Sum256([]byte(pattern))
	return hex.EncodeToString(sum[:])
}
