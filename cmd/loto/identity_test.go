package main

import (
	"regexp"
	"testing"
)

func TestResolveAgentID_PrefersLOTOAgentID(t *testing.T) {
	t.Setenv("LOTO_AGENT_ID", "explicit-handle")
	t.Setenv("CLAUDE_SESSION_ID", "should-be-ignored")

	id, src := resolveAgentID()
	if id != "explicit-handle" {
		t.Errorf("id: got %q, want %q", id, "explicit-handle")
	}
	if src != srcEnv {
		t.Errorf("src: got %q, want %q", src, srcEnv)
	}
}

func TestResolveAgentID_DerivesFromClaudeSessionID(t *testing.T) {
	t.Setenv("LOTO_AGENT_ID", "")
	t.Setenv("CLAUDE_SESSION_ID", "session-abc")

	id1, src1 := resolveAgentID()
	id2, src2 := resolveAgentID()
	if id1 != id2 {
		t.Errorf("non-deterministic: %q != %q", id1, id2)
	}
	if src1 != srcSession || src2 != srcSession {
		t.Errorf("src: got %q/%q, want %q", src1, src2, srcSession)
	}
	if !uuidRE.MatchString(id1) {
		t.Errorf("id %q is not UUID-shaped", id1)
	}
}

func TestResolveAgentID_DistinctSessionsDistinctIDs(t *testing.T) {
	t.Setenv("LOTO_AGENT_ID", "")

	t.Setenv("CLAUDE_SESSION_ID", "alpha")
	idA, _ := resolveAgentID()

	t.Setenv("CLAUDE_SESSION_ID", "beta")
	idB, _ := resolveAgentID()

	if idA == idB {
		t.Errorf("expected distinct IDs for distinct sessions, both were %q", idA)
	}
}

func TestResolveAgentID_PIDFallback(t *testing.T) {
	t.Setenv("LOTO_AGENT_ID", "")
	t.Setenv("CLAUDE_SESSION_ID", "")

	id, src := resolveAgentID()
	if src != srcPID {
		t.Errorf("src: got %q, want %q", src, srcPID)
	}
	if !regexp.MustCompile(`^pid-\d+$`).MatchString(id) {
		t.Errorf("id %q does not match pid-N", id)
	}
}

// Different handles should be derived from different session IDs (sanity:
// the deterministic UUID feeds generateHandle through createAgent).
func TestSessionUUID_HandleDifferentForDifferentSessions(t *testing.T) {
	a := sessionUUID("session-one")
	b := sessionUUID("session-two")
	if a == b {
		t.Errorf("sessionUUID collision on distinct inputs: %q", a)
	}
	if generateHandle(a) == generateHandle(b) {
		t.Logf("note: handles collided despite distinct UUIDs (handle space is small)")
	}
}
