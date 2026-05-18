package identity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestLookupByUUIDMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if _, err := LookupByUUID("does-not-exist"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestLookupByUUIDEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if _, err := LookupByUUID(""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty uuid must miss, got %v", err)
	}
}

func TestLookupByUUIDMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path := filepath.Join(dir, ".loto", "agents", "bad.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LookupByUUID("bad")
	if err == nil {
		t.Fatal("want decode error, got nil")
	}
	var se *json.SyntaxError
	if !errors.As(err, &se) {
		t.Fatalf("want json.SyntaxError, got %v", err)
	}
}

func TestLookupByUUIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	want := Agent{
		UUID:      "agent-123",
		Handle:    "SwiftFalcon",
		Host:      "devbox",
		CreatedAt: time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC),
	}
	body, err := json.Marshal(&want)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".loto", "agents", want.UUID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	// Sibling non-JSON file must not interfere.
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LookupByUUID(want.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UUID != want.UUID || got.Handle != want.Handle || got.Host != want.Host || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("round-trip mismatch: want %+v got %+v", want, *got)
	}
}

func TestNewUUIDFormatAndUniqueness(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	const n = 32
	seen := make(map[string]struct{}, n)
	for range n {
		u := NewUUID()
		if !re.MatchString(u) {
			t.Fatalf("uuid %q does not match v4 pattern", u)
		}
		if _, dup := seen[u]; dup {
			t.Fatalf("duplicate uuid: %s", u)
		}
		seen[u] = struct{}{}
	}
}
