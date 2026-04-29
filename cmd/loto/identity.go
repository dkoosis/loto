package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Agent represents a session-persistent loto identity.
// Stored at ~/.loto/agents/<uuid>.json; shared via LOTO_AGENT_ID env var.
type Agent struct {
	ID        string    `json:"id"`     // UUID
	Handle    string    `json:"handle"` // adjective+noun PascalCase (placeholder for .10)
	CreatedAt time.Time `json:"created_at"`
	Host      string    `json:"host,omitempty"`
}

// agentHome returns ~/.loto/agents/.
func agentHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("loto: resolve home dir: %w", err)
	}
	dir := filepath.Join(home, ".loto", "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("loto: create agent home: %w", err)
	}
	return dir, nil
}

// currentAgent returns the agent for this session.
// If LOTO_AGENT_ID is set, loads that agent file.
// Otherwise creates a new agent and writes it to disk.
func currentAgent() (*Agent, error) {
	dir, err := agentHome()
	if err != nil {
		return nil, err
	}

	if id := os.Getenv("LOTO_AGENT_ID"); id != "" {
		path := filepath.Join(dir, id+".json")
		data, err := os.ReadFile(path) //nolint:gosec // G304/G703: path under XDG identity dir, id from env
		if err == nil {
			var a Agent
			if err := json.Unmarshal(data, &a); err == nil {
				return &a, nil
			}
		}
		// LOTO_AGENT_ID set but file missing/corrupt — create a new one with that ID.
		return createAgent(dir, id)
	}

	return createAgent(dir, newUUID())
}

func createAgent(dir, id string) (*Agent, error) {
	host, _ := os.Hostname()
	a := &Agent{
		ID:        id,
		Handle:    generateHandle(id),
		CreatedAt: time.Now().UTC(),
		Host:      host,
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("loto: marshal agent: %w", err)
	}
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // G304/G703: path under XDG identity dir
		return nil, fmt.Errorf("loto: write agent file: %w", err)
	}
	return a, nil
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
