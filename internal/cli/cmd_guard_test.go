package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// TestGuardPeerRows_FailOpenNoticesGoToStderr pins loto-tzmv.8 on the guard
// leg. `loto guard` is installed as a git alias, so its fail-open notice is
// the only signal that a tree-move went unchecked; on stdout it is swallowed
// by whatever consumes the wrapped git verb's output. Both fail-open branches
// (unpinned identity, unreachable store) must write to stderr.
func TestGuardPeerRows_FailOpenNoticesGoToStderr(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{
			name: "unpinned identity",
			setup: func(t *testing.T) {
				t.Helper()
				os.Unsetenv("LOTO_AGENT_ID")
				os.Unsetenv("LOTO_SUBAGENT_ID")
				os.Unsetenv("CLAUDE_CODE_SESSION_ID")
			},
			want: "⚠ identity=unpinned guard=fail-open",
		},
		{
			name: "store unreachable",
			setup: func(t *testing.T) {
				t.Helper()
				pinAgent(t)
				t.Setenv("LOTO_BASE", brokenLOTOBase(t))
			},
			want: "⚠ store=unreachable guard=fail-open",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTempProject(t)
			tt.setup(t)

			var stderr bytes.Buffer
			rows, failOpen := guardPeerRows(context.Background(), &stderr)
			if !failOpen {
				t.Fatalf("expected fail-open, got rows=%v", rows)
			}
			if rows != nil {
				t.Errorf("fail-open must carry no rows, got %v", rows)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("expected %q on stderr, got %q", tt.want, stderr.String())
			}
		})
	}
}
