package loto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

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
		return nil, &ErrSystem{Op: "reserve: invalid pattern", Err: fmt.Errorf("invalid glob pattern: %q", pattern)}
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
	tagPath := filepath.Join(resDir, hashPattern(pattern)+".tag")
	if err := os.WriteFile(tagPath, append(data, '\n'), 0o600); err != nil {
		return nil, &ErrSystem{Op: "reserve: write", Err: err}
	}
	return r, nil
}

// Unreserve removes the reservation for the given pattern, if it exists.
func (l *LOTO) Unreserve(pattern string) error {
	tagPath := filepath.Join(l.reservationsDir(), hashPattern(pattern)+".tag")
	if err := os.Remove(tagPath); err != nil && !os.IsNotExist(err) {
		return &ErrSystem{Op: "unreserve: remove", Err: err}
	}
	return nil
}

// ListReservations returns all active (non-expired) reservations.
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
		if filepath.Ext(e.Name()) != ".tag" {
			continue
		}
		r, err := l.readReservation(filepath.Join(resDir, e.Name()))
		if err != nil || r == nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
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
		return nil, nil
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

// ErrReservationConflict is returned when TryFileLock detects a conflicting
// advisory reservation and the policy is set to fail (future work).
// For now, conflicts are surface as warnings only.
type ErrReservationConflict struct {
	Target       string
	Reservations []*Reservation
}

func (e *ErrReservationConflict) Error() string {
	if len(e.Reservations) == 1 {
		r := e.Reservations[0]
		return fmt.Sprintf("loto: %s conflicts with reservation %q held by %s (%s)",
			e.Target, r.Pattern, r.AgentID, r.Intent)
	}
	return fmt.Sprintf("loto: %s conflicts with %d reservations", e.Target, len(e.Reservations))
}
