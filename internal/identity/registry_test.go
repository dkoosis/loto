package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureIdentityCreatesRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOTO_AGENT_ID", "")

	a, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID == "" || a.Handle == "" {
		t.Fatalf("agent missing fields: %+v", a)
	}
	path := filepath.Join(dir, ".loto", "agents", a.UUID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("identity file missing: %v", err)
	}
}

func TestEnsureRespectsExistingEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	first, _ := Ensure()
	t.Setenv("LOTO_AGENT_ID", first.UUID)
	second, _ := Ensure()
	if second.UUID != first.UUID {
		t.Fatalf("Ensure() must return same uuid when LOTO_AGENT_ID is set; %s != %s", second.UUID, first.UUID)
	}
}

func TestResolveHandleByUUID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	a, _ := Ensure()
	got, err := resolveByHandle(a.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if got.UUID != a.UUID {
		t.Errorf("resolveByHandle: got %s want %s", got.UUID, a.UUID)
	}
}
