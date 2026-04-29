package render

import (
	"os"
	"testing"
)

func TestResolveExplicitJSON(t *testing.T) {
	if got := Resolve("json", os.Stdout); got != FormatJSON {
		t.Fatalf("explicit json: got %v want FormatJSON", got)
	}
}

func TestResolveExplicitLLM(t *testing.T) {
	if got := Resolve("llm", os.Stdout); got != FormatLLM {
		t.Fatalf("explicit llm: got %v want FormatLLM", got)
	}
}

func TestResolveAutoNonTTY(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if got := Resolve("", w); got != FormatLLM {
		t.Fatalf("auto non-tty: got %v want FormatLLM", got)
	}
}
