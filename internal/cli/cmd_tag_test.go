package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestTagAndUntag(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	if code := Run([]string{"tag", tcTargetA, "-t", "ping me"}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("tag failed: %d %q", code, out.String())
	}
	if !strings.Contains(out.String(), "t-") {
		t.Errorf("expected tag id; got %q", out.String())
	}
	id := extractTagID(out.String())
	out.Reset()
	if code := Run([]string{"untag", tcTargetA, id}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("untag failed: %d %q", code, out.String())
	}
}

func extractTagID(s string) string {
	for f := range strings.FieldsSeq(s) {
		if strings.HasPrefix(f, "t-") {
			return f
		}
		// Look for tag=t-... form too.
		if strings.HasPrefix(f, "tag=t-") {
			return strings.TrimPrefix(f, "tag=")
		}
	}
	return ""
}
