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
	"strings"
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
	if err := os.WriteFile(tagPath, append(data, '\n'), 0o600); err != nil {
		return nil, &ErrSystem{Op: "reserve: write", Err: err}
	}
	return r, nil
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

// OverlappingReservations returns active reservations whose pattern overlaps
// with the given pattern. Symmetric: overlap(a,b) == overlap(b,a). Used to
// surface advisory warnings when staking a new reservation that intersects
// existing ones — never blocks (reservations remain advisory).
func (l *LOTO) OverlappingReservations(pattern string) ([]*Reservation, error) {
	if !doublestar.ValidatePattern(pattern) {
		return nil, &ErrSystem{Op: "overlap: invalid pattern", Err: fmt.Errorf("%w: %q", ErrInvalidGlob, pattern)}
	}
	all, err := l.ListReservations()
	if err != nil {
		return nil, err
	}
	var overlaps []*Reservation
	for _, r := range all {
		if patternsOverlap(pattern, r.Pattern) {
			overlaps = append(overlaps, r)
		}
	}
	return overlaps, nil
}

// patternsOverlap reports whether two doublestar globs could match at least
// one common path. Symmetric. Conservative — false positives are advisory
// (never block), false negatives would silently miss conflicts so we err
// toward reporting.
//
// Algorithm:
//  1. Identical patterns trivially overlap.
//  2. If either pattern matches the other treated as a literal name, overlap
//     (handles cases like internal/** vs internal/store/**).
//  3. Otherwise compare literal prefixes (chars before first glob meta). If
//     one prefix is a path-prefix of the other, the meta-tails could match
//     a common path — report overlap. Disjoint prefixes → no overlap.
func patternsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	if ok, _ := doublestar.Match(a, b); ok {
		return true
	}
	if ok, _ := doublestar.Match(b, a); ok {
		return true
	}
	prefA := literalPrefix(a)
	prefB := literalPrefix(b)
	return isPathPrefix(prefA, prefB) || isPathPrefix(prefB, prefA)
}

// literalPrefix returns the longest path-segment prefix of pattern containing
// no glob metacharacters. "internal/store/**" → "internal/store"; "**/foo.go"
// → ""; "pkg/a*.go" → "pkg".
func literalPrefix(pattern string) string {
	segs := strings.Split(pattern, "/")
	var literal []string
	for _, s := range segs {
		if strings.ContainsAny(s, "*?[{") {
			break
		}
		literal = append(literal, s)
	}
	return strings.Join(literal, "/")
}

// isPathPrefix reports whether prefix is a path-prefix of full (or equal).
// Empty prefix is a path-prefix of anything.
func isPathPrefix(prefix, full string) bool {
	if prefix == "" {
		return true
	}
	if prefix == full {
		return true
	}
	return strings.HasPrefix(full, strings.TrimSuffix(prefix, "/")+"/")
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
