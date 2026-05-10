package cli

import (
	"flag"
	"strings"
)

// permuteWith rearranges args for fs so flags can appear after positional
// arguments. Walks left-to-right, distinguishes value-taking flags from bool
// flags by inspecting the FlagSet definition. Returns flags first, positional
// last — flag.Parse can then handle the result with default semantics.
func permuteWith(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			positional = append(positional, a)
			continue
		}
		// Strip leading dashes for lookup; preserve original token in output.
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			_ = eq // value is embedded in token; flag.Parse will split it
			flags = append(flags, a)
			continue
		}
		flags = append(flags, a)
		// Look up the flag to decide if it expects a value.
		f := fs.Lookup(name)
		if f == nil {
			// Unknown flag — let flag.Parse error out cleanly. Don't consume next.
			continue
		}
		if isBoolFlag(f) {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	out := make([]string, 0, len(args))
	out = append(out, flags...)
	out = append(out, positional...)
	return out
}

func isBoolFlag(f *flag.Flag) bool {
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
		return bf.IsBoolFlag()
	}
	return false
}
