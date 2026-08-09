package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tempBeadWarnMarker points the once-per-session witness at a temp dir so a
// test never claims (or is blocked by) the real one.
func tempBeadWarnMarker(t *testing.T, present bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "marker")
	prev := beadWarnMarker
	beadWarnMarker = func() (string, bool) { return path, present }
	t.Cleanup(func() { beadWarnMarker = prev })
}

func TestWarnIfNoBeadIDFiresOncePerSession(t *testing.T) {
	tempBeadWarnMarker(t, true)

	var first, second bytes.Buffer
	warnIfNoBeadID("msg", "want next", &first)
	warnIfNoBeadID("msg", "still want next", &second)

	if !strings.Contains(first.String(), "bead id") {
		t.Fatalf("first call must teach the convention: %q", first.String())
	}
	if second.Len() != 0 {
		t.Fatalf("second call must stay quiet; got %q", second.String())
	}
}

func TestWarnIfNoBeadIDNamesTheSurface(t *testing.T) {
	// The convention warning is shared by tag and msg; it must not tell a msg
	// sender about "tag text" (loto-2hl0).
	for _, kind := range []string{"tag", "msg"} {
		t.Run(kind, func(t *testing.T) {
			tempBeadWarnMarker(t, true)
			var out bytes.Buffer
			warnIfNoBeadID(kind, "want next", &out)
			if !strings.HasPrefix(out.String(), "∇ "+kind+" text") {
				t.Fatalf("want a %s-flavored warning, got %q", kind, out.String())
			}
		})
	}
}

func TestWarnIfNoBeadIDSilentWithPrefix(t *testing.T) {
	tempBeadWarnMarker(t, true)
	var out bytes.Buffer
	warnIfNoBeadID("msg", "loto-2hl0: who table", &out)
	if out.Len() != 0 {
		t.Fatalf("a bead-id prefix must not warn; got %q", out.String())
	}
}

func TestWarnIfNoBeadIDAlwaysWarnsWithoutSession(t *testing.T) {
	// A bare human shell has no session id, so there is nothing to be "once"
	// per — the nudge stays on every invocation.
	tempBeadWarnMarker(t, false)
	for i := range 2 {
		var out bytes.Buffer
		warnIfNoBeadID("tag", "want next", &out)
		if !strings.Contains(out.String(), "bead id") {
			t.Fatalf("call %d must warn without a session marker: %q", i, out.String())
		}
	}
}

func TestDefaultBeadWarnMarkerRejectsPathShapedSessionID(t *testing.T) {
	t.Setenv("LOTO_SESSION_ID", "../escape")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	if _, ok := defaultBeadWarnMarker(); ok {
		t.Fatal("a path-shaped session id must fall open, not build a marker path")
	}

	t.Setenv("LOTO_SESSION_ID", "sess-1")
	path, ok := defaultBeadWarnMarker()
	if !ok || !strings.HasSuffix(path, "loto-beadwarn-sess-1") {
		t.Fatalf("marker = %q, ok = %v", path, ok)
	}
}
