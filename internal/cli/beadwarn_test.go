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
	warnIfNoBeadID("want next", &first)
	warnIfNoBeadID("still want next", &second)

	if !strings.Contains(first.String(), "bead id") {
		t.Fatalf("first call must teach the convention: %q", first.String())
	}
	if second.Len() != 0 {
		t.Fatalf("second call must stay quiet; got %q", second.String())
	}
}

func TestWarnIfNoBeadIDNamesTheSurface(t *testing.T) {
	// tag is the only surface carrying free-text coordination now that mail is
	// retired (loto-3wlb), so the warning names it outright (loto-2hl0).
	tempBeadWarnMarker(t, true)
	var out bytes.Buffer
	warnIfNoBeadID("want next", &out)
	if !strings.HasPrefix(out.String(), "∇ tag text") {
		t.Fatalf("want a tag-flavored warning, got %q", out.String())
	}
}

func TestWarnIfNoBeadIDSilentWithPrefix(t *testing.T) {
	tempBeadWarnMarker(t, true)
	var out bytes.Buffer
	warnIfNoBeadID("loto-2hl0: who table", &out)
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
		warnIfNoBeadID("want next", &out)
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
