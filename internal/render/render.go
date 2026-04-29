// Package render emits loto command output in either JSON (machine/legacy)
// or LLM (token-dense, Claude-optimized) form. See nug 32f0ece29b72.
package render

import (
	"encoding/json"
	"io"
	"os"
)

type Format int

const (
	FormatJSON Format = iota
	FormatLLM
)

// Resolve picks the output format based on explicit user choice and tty state.
// explicit: "json" | "llm" | "" (auto). When auto, non-tty → LLM, tty → JSON.
func Resolve(explicit string, stdout *os.File) Format {
	switch explicit {
	case "json":
		return FormatJSON
	case "llm":
		return FormatLLM
	}
	if fi, err := stdout.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		return FormatLLM
	}
	return FormatJSON
}

// EmitJSON writes v as indented JSON to w.
func EmitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
