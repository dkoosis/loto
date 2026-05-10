package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var stdout bytes.Buffer
	code := Run([]string{"version"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout.String(), "rev=") {
		t.Errorf("expected rev= token: %q", stdout.String())
	}
}
