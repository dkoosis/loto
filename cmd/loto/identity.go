package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
// Resolution order: persisted Agent.Handle (if the agent file exists) →
// deterministic handle for UUID-shaped IDs → passthrough.
func displayAgent(id string) string {
	if id == "" {
		return id
	}
	if h := lookupAgentHandle(id); h != "" {
		return h
	}
	if uuidRE.MatchString(id) {
		return generateHandle(id)
	}
	return id
}

// lookupAgentHandle reads the persisted Handle for id, if its agent file
// exists. Returns "" on any miss; never errors out — display must be
// best-effort and never block on disk problems.
func lookupAgentHandle(id string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".loto", "agents", id+".json"))
	if err != nil {
		return ""
	}
	var a Agent
	if err := json.Unmarshal(data, &a); err != nil {
		return ""
	}
	return a.Handle
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
//  3. shell-<hash(PPID|CWD|host)> — stable for non-Claude shells: every
//     loto invocation from the same shell + same project converges on one
//     identity. PPID reuse after the parent dies is mitigated by including
//     CWD in the hash.
func resolveAgentID() (string, agentIDSource) {
	if id := os.Getenv("LOTO_AGENT_ID"); id != "" {
		return id, srcEnv
	}
	if sid := discoverCCSessionID(); sid != "" {
		return sessionUUID(sid), srcSession
	}
	return shellAgentID(), srcPID
}

// shellAgentID derives a stable per-shell, per-project identity. Same parent
// shell + same working directory + same host → same ID across loto invocations.
func shellAgentID() string {
	cwd, _ := os.Getwd()
	host, _ := os.Hostname()
	key := fmt.Sprintf("%d|%s|%s", os.Getppid(), cwd, host)
	sum := sha256.Sum256([]byte("loto-shell-v1|" + key))
	return "shell-" + hex.EncodeToString(sum[:6])
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
// record on first use. All sources (env, session, shell-key) re-attach to
// an existing agent file when one is present, so repeated invocations from
// the same shell or session converge on a single identity.
func currentAgent() (*Agent, error) {
	dir, err := agentHome()
	if err != nil {
		return nil, err
	}

	id, _ := resolveAgentID()
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
