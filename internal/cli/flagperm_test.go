package cli

import (
	"flag"
	"reflect"
	"testing"
)

// Permute-test fixtures, named to satisfy goconst (each literal otherwise
// recurs ≥3× across the cli test files).
const (
	tcIntentWhy     = "why"
	tcDashFileX     = "-x.go"
	tcDashFileWeird = "-weird.go"
)

// newLockFS mirrors the FlagSet shape that cmdLock builds, so permute tests
// exercise realistic flag definitions.
func newLockFS() *flag.FlagSet {
	fs := flag.NewFlagSet("lock", flag.ContinueOnError)
	fs.Duration("ttl", 0, "lock TTL")
	intent := fs.String("t", "", "intent")
	fs.StringVar(intent, "intent", "", "intent")
	return fs
}

func TestPermuteWith_EndOfFlagsEscape(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			// Acceptance: a leading-dash target after -- stays a positional.
			name: "double-dash then dash file",
			args: []string{"--", tcDashFileX},
			want: []string{"--", tcDashFileX},
		},
		{
			name: "flag then double-dash then dash files",
			args: []string{"-t", tcIntentWhy, "--", tcDashFileWeird, "-also.go"},
			want: []string{"-t", tcIntentWhy, "--", tcDashFileWeird, "-also.go"},
		},
		{
			// Tokens after -- are never treated as flags, even known ones.
			name: "known flag name after double-dash is positional",
			args: []string{"--", "-t", tcDashFileX},
			want: []string{"--", "-t", tcDashFileX},
		},
		{
			name: "no escape leaves normal permute behavior",
			args: []string{"foo", "-t", "why"},
			want: []string{"-t", "why", "foo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := permuteWith(newLockFS(), tt.args)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("permuteWith(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestPermuteWith_ParseReachesPositional asserts the end-to-end contract:
// fs.Parse(permuteWith(fs, args)) must surface a leading-dash target as a
// positional arg rather than erroring on an unknown flag.
func TestPermuteWith_ParseReachesPositional(t *testing.T) {
	fs := newLockFS()
	if err := fs.Parse(permuteWith(fs, []string{"-t", tcIntentWhy, "--", tcDashFileWeird})); err != nil {
		t.Fatalf("Parse errored: %v", err)
	}
	if got := fs.Args(); !reflect.DeepEqual(got, []string{tcDashFileWeird}) {
		t.Fatalf("fs.Args() = %v, want [-weird.go]", got)
	}
	if got := fs.Lookup("t").Value.String(); got != tcIntentWhy {
		t.Fatalf("intent = %q, want \"why\"", got)
	}
}
