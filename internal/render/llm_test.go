package render

import (
	"bytes"
	"strings"
	"testing"
)

func TestEmitLLMWhoami(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMWhoami(&buf, "2dd46381-9c26-4c01-97ce-91beda0103d1", "RemoteSnipe", "Mac"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "loto:llm:v1\n") {
		t.Fatalf("missing header; got:\n%s", got)
	}
	if !strings.Contains(got, "agent | RemoteSnipe | id:2dd46381 | host:Mac\n") {
		t.Fatalf("unexpected body:\n%s", got)
	}
}
