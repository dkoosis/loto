package cli

import (
	"flag"
	"reflect"
	"testing"
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
			name: "dash dash then dash file",
			args: []string{"--", "-x.go"},
			want: []string{"--", "-x.go"},
		},
		{
			name: "flag then dash dash then dash files",
			args: []string{"-t", "why", "--", "-weird.go", "-also.go"},
			want: []string{"-t", "why", "--", "-weird.go", "-also.go"},
		},
		{
			// Tokens after -- are never treated as flags, even known ones.
			name: "known flag name after dash dash is positional",
			args: []string{"--", "-t", "-x.go"},
			want: []string{"--", "-t", "-x.go"},
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
	if err := fs.Parse(permuteWith(fs, []string{"-t", "why", "--", "-weird.go"})); err != nil {
		t.Fatalf("Parse errored: %v", err)
	}
	if got := fs.Args(); !reflect.DeepEqual(got, []string{"-weird.go"}) {
		t.Fatalf("fs.Args() = %v, want [-weird.go]", got)
	}
	if got := fs.Lookup("t").Value.String(); got != "why" {
		t.Fatalf("intent = %q, want \"why\"", got)
	}
}
