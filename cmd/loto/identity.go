package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// displayAgent resolves a raw agent ID to a human-readable handle for LLM/tty output.
// UUID-shaped IDs become deterministic handles (e.g. "GreenCastle"). Anything else
// (e.g. "pid-1234", legacy literal handles) passes through unchanged.
func displayAgent(id string) string {
	if uuidRE.MatchString(id) {
		return generateHandle(id)
	}
	return id
}

// Agent represents a session-persistent loto identity.
// Stored at ~/.loto/agents/<uuid>.json; resolved per process via
// resolveAgentID (LOTO_AGENT_ID → CC session JSONL → pid-N).
type Agent struct {
	ID        string    `json:"id"`     // UUID
	Handle    string    `json:"handle"` // adjective+noun PascalCase
	CreatedAt time.Time `json:"created_at"`
	Host      string    `json:"host,omitempty"`
}

// agentIDSource describes how the current agent ID was resolved.
type agentIDSource string

const (
	srcEnv     agentIDSource = "env"
	srcSession agentIDSource = "session"
	srcPID     agentIDSource = "pid"
)

// resolveAgentID returns the stable agent ID for this process and its source.
// Resolution order:
//  1. LOTO_AGENT_ID — explicit override / re-attach
//  2. CC session JSONL discovery — deterministic per-session UUID; every
//     shell-out within one Claude session converges on the same handle without
//     env-var injection.
//  3. pid-N — ephemeral fallback for non-Claude shells.
func resolveAgentID() (string, agentIDSource) {
	if id := os.Getenv("LOTO_AGENT_ID"); id != "" {
		return id, srcEnv
	}
	if sid := discoverCCSessionID(); sid != "" {
		return sessionUUID(sid), srcSession
	}
	return fmt.Sprintf("pid-%d", os.Getpid()), srcPID
}

// discoverCCSessionID finds the active Claude Code session ID by reading
// ~/.claude/projects/<encoded-cwd>/*.jsonl and picking the newest file.
// LOTO_CC_PROJECT_DIR overrides the lookup directory (for tests).
func discoverCCSessionID() string {
	projectDir := os.Getenv("LOTO_CC_PROJECT_DIR")
	if projectDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		cwd, err := os.Getwd()
		if err != nil {
			return ""
		}
		// CC encodes cwd by replacing "/" with "-":
		// /Users/vcto/Projects/loto → -Users-vcto-Projects-loto
		encoded := strings.ReplaceAll(cwd, string(filepath.Separator), "-")
		projectDir = filepath.Join(home, ".claude", "projects", encoded)
	}
	return discoverCCSessionIDFrom(projectDir)
}

// discoverCCSessionIDFrom returns the session ID (filename sans .jsonl) of
// the most recently modified JSONL file in dir. Returns "" if none found.
func discoverCCSessionIDFrom(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if t := info.ModTime().UnixNano(); t > newestTime {
			newestTime = t
			newest = e.Name()
		}
	}
	if newest == "" {
		return ""
	}
	return strings.TrimSuffix(newest, ".jsonl")
}

// sessionUUID derives a deterministic v4-shaped UUID from a CC session ID.
// Same session ID → same UUID → same agent file → same handle.
func sessionUUID(sessionID string) string {
	sum := sha256.Sum256([]byte("loto-session-v1|" + sessionID))
	var b [16]byte
	copy(b[:], sum[:])
	return uuidFromBytes(b)
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

// currentAgent returns the agent for this session, creating its on-disk
// record on first use. For srcEnv/srcSession the ID is stable across calls;
// for srcPID each call gets a fresh random UUID.
func currentAgent() (*Agent, error) {
	dir, err := agentHome()
	if err != nil {
		return nil, err
	}

	id, src := resolveAgentID()
	if src == srcPID {
		return createAgent(dir, newUUID())
	}

	path := filepath.Join(dir, id+".json")
	data, err := os.ReadFile(path)
	if err == nil {
		var a Agent
		if err := json.Unmarshal(data, &a); err == nil {
			return &a, nil
		}
	}
	return createAgent(dir, id)
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
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, fmt.Errorf("loto: write agent file: %w", err)
	}
	return a, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return uuidFromBytes(b)
}

// uuidFromBytes formats 16 bytes as a v4-shaped UUID per RFC 4122.
func uuidFromBytes(b [16]byte) string {
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
