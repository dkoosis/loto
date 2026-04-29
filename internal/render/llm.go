package render

import (
	"fmt"
	"io"
)

const llmHeader = "loto:llm:v1\n"

// shortID returns the first 8 chars of a UUID-ish string for display.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// EmitLLMWhoami writes the whoami output in LLM format.
// Layout: "agent | <handle> | id:<short> | host:<host>"
func EmitLLMWhoami(w io.Writer, id, handle, host string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "agent | %s | id:%s | host:%s\n", handle, shortID(id), host)
	return err
}
