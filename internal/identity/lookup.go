package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoSuchHandle reports a handle with no matching agent record.
var ErrNoSuchHandle = errors.New("no agent with that handle")

// LookupByHandle resolves a display handle (e.g. "SunnyPorcupine") to its
// agent record by scanning the registry dir. Handles are minted from a small
// wordlist so collisions are possible in principle; on a duplicate the
// newest record wins (latest CreatedAt) — the older one is GC-bound anyway.
// Matching is case-insensitive: handles are PascalCase display names, and a
// sender typing "sunnyporcupine" should not miss.
func LookupByHandle(handle string) (*Agent, error) {
	entries, err := os.ReadDir(registryDir())
	if err != nil {
		return nil, fmt.Errorf("agent registry: %w", err)
	}
	var best *Agent
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(registryDir(), e.Name()))
		if err != nil {
			continue
		}
		var a Agent
		if err := json.Unmarshal(body, &a); err != nil {
			continue
		}
		if !strings.EqualFold(a.Handle, handle) {
			continue
		}
		if best == nil || a.CreatedAt.After(best.CreatedAt) {
			cp := a
			best = &cp
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoSuchHandle, handle)
	}
	return best, nil
}
