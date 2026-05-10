package identity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrAgentNotFound = errors.New("no such agent on this host")

type Agent struct {
	UUID      string    `json:"uuid"`
	Handle    string    `json:"handle"`
	CreatedAt time.Time `json:"created_at"`
	Host      string    `json:"host"`
}

func registryDir() string {
	return filepath.Join(os.Getenv("HOME"), ".loto", "agents")
}

// Ensure returns the current session's agent. If LOTO_AGENT_ID is set and
// resolves, returns it. Otherwise creates a new identity, writes it.
func Ensure() (*Agent, error) {
	u, set := os.LookupEnv("LOTO_AGENT_ID")
	if set && u != "" {
		if a, err := loadByUUID(u); err == nil {
			return a, nil
		}
	}
	// Empty-but-set LOTO_AGENT_ID means "force new agent" (test fixtures use
	// this to spin distinct identities). Unset means "interactive shell, no
	// hook" — fall back to the most recent agent on this host so lock/unlock
	// can pair across invocations.
	if !set {
		if a, err := mostRecentAgent(); err == nil && a != nil {
			return a, nil
		}
	}
	return newAgent()
}

func mostRecentAgent() (*Agent, error) {
	entries, err := os.ReadDir(registryDir())
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	var best *Agent
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
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
		if a.Host != host {
			continue
		}
		if best == nil || a.CreatedAt.After(best.CreatedAt) {
			cp := a
			best = &cp
		}
	}
	return best, nil
}

func newAgent() (*Agent, error) {
	if err := os.MkdirAll(registryDir(), 0o700); err != nil {
		return nil, err
	}
	uuid := newUUID()
	handle := randomHandle()
	host, _ := os.Hostname()
	a := &Agent{UUID: uuid, Handle: handle, CreatedAt: time.Now().UTC(), Host: host}
	if err := writeAgent(a); err != nil {
		return nil, err
	}
	return a, nil
}

func loadByUUID(uuid string) (*Agent, error) {
	path := filepath.Join(registryDir(), uuid+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a Agent
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func resolveByHandle(handle string) (*Agent, error) {
	entries, err := os.ReadDir(registryDir())
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
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
		if a.Handle == handle {
			return &a, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, handle)
}

// Resolve accepts either a uuid or a handle.
func Resolve(s string) (*Agent, error) {
	if a, err := loadByUUID(s); err == nil {
		return a, nil
	}
	return resolveByHandle(s)
}

func writeAgent(a *Agent) error {
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(registryDir(), a.UUID+".json"), body, 0o600)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}
