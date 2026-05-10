package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"loto"
	"loto/internal/render"
)

const defaultTiebreaker = "msg+switch>2min"

// usageExit writes a usage error to stderr and exits 2. Mirrors the pattern
// used by release/inbox for flag-shape violations.
func usageExit(msg string) {
	fmt.Fprintln(os.Stderr, "loto: hello: "+msg)
	os.Exit(2)
}

func helloCmd() *cobra.Command {
	var (
		to           string
		ttl          string
		tiebreaker   string
		noTiebreaker bool
	)

	c := &cobra.Command{
		Use:   "hello <glob>",
		Short: "reserve a glob and announce it to siblings with a parseable body",
		Long: `hello atomically reserves a glob and sends a stable, parseable announcement
to each named sibling. The body uses fixed pipe-delimited fields so siblings
can grep/parse it without prose drift:

  loto:llm:v1 hello | handle:<self> | glob:<glob> | intent:<intent> | tiebreaker:<tb>

--to may be a comma-separated list. With --to empty, hello only reserves the
glob (no msgs sent). Default tiebreaker is "` + defaultTiebreaker + `";
--no-tiebreaker omits the field.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			glob := args[0]
			intent := flagIntent

			if tiebreaker != "" && noTiebreaker {
				usageExit("--tiebreaker and --no-tiebreaker are mutually exclusive")
			}
			if intent == "" {
				usageExit("--intent is required")
			}
			tb := tiebreaker
			if tb == "" && !noTiebreaker {
				tb = defaultTiebreaker
			}
			for _, f := range []string{glob, intent, tb} {
				if strings.Contains(f, "|") {
					usageExit("glob, intent, and tiebreaker must not contain '|'")
				}
			}

			l := newLOTO()

			var dur time.Duration
			if ttl != "" {
				dur = parseDurationOrExit("ttl", ttl)
			}
			if _, err := l.Reserve(flagAgent, intent, glob, dur); err != nil {
				exit(err)
			}

			// validateHandle (cmd/loto/identity.go) rejects '|' at set-handle
			// time, so any handle reaching this point — set via whoami or
			// auto-derived from the agent ID — is already separator-safe.
			selfHandle := selfHandleOrID()
			body := buildHelloBody(selfHandle, glob, intent, tb, noTiebreaker)

			recipients := splitRecipients(to)
			results := make([]render.HelloRecipient, 0, len(recipients))
			anyFailed := false
			for _, h := range recipients {
				err := l.SendMsgWith(glob, loto.Msg{
					From: flagAgent,
					To:   h,
					Body: body,
				})
				rr := render.HelloRecipient{Handle: h, Sent: err == nil}
				if err != nil {
					rr.Error = err.Error()
					anyFailed = true
				}
				results = append(results, rr)
			}
			sort.SliceStable(results, func(i, j int) bool {
				if results[i].Sent != results[j].Sent {
					return results[i].Sent // sent first
				}
				return results[i].Handle < results[j].Handle
			})

			emitHello(render.HelloResult{
				Glob:       glob,
				Intent:     intent,
				Agent:      flagAgent,
				Handle:     selfHandle,
				Recipients: results,
			})
			if anyFailed {
				os.Exit(1)
			}
			return nil
		},
	}

	c.Flags().StringVar(&to, "to", "", "comma-separated sibling handles to notify (empty = reserve only)")
	c.Flags().StringVar(&ttl, "ttl", "", "advisory expiry on the reservation (e.g. 30m, 2h)")
	c.Flags().StringVar(&tiebreaker, "tiebreaker", "", "tiebreaker hint embedded in the body (default: "+defaultTiebreaker+")")
	c.Flags().BoolVar(&noTiebreaker, "no-tiebreaker", false, "omit the tiebreaker field entirely")
	return c
}

// selfHandleOrID returns the handle for flagAgent if a record exists, else
// flagAgent itself. The persistent --agent flag (defaulting to the auto-
// resolved session ID) is authoritative for this invocation.
func selfHandleOrID() string {
	dir, err := agentHome()
	if err != nil {
		return flagAgent
	}
	data, err := os.ReadFile(filepath.Join(dir, flagAgent+".json"))
	if err != nil {
		return flagAgent
	}
	var a Agent
	if err := json.Unmarshal(data, &a); err == nil && a.Handle != "" {
		return a.Handle
	}
	return flagAgent
}

// buildHelloBody composes the pipe-delimited stable hello body. Field order is
// fixed; --no-tiebreaker omits the trailing tiebreaker field.
func buildHelloBody(handle, glob, intent, tiebreaker string, omitTiebreaker bool) string {
	parts := []string{
		"loto:llm:v1 hello",
		"handle:" + handle,
		"glob:" + glob,
		"intent:" + intent,
	}
	if !omitTiebreaker {
		parts = append(parts, "tiebreaker:"+tiebreaker)
	}
	return strings.Join(parts, " | ")
}

// splitRecipients turns "a,b, ,c" into [a b c].
func splitRecipients(spec string) []string {
	if spec == "" {
		return nil
	}
	out := make([]string, 0, strings.Count(spec, ",")+1)
	for p := range strings.SplitSeq(spec, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func emitHello(r render.HelloResult) {
	if currentFormat == render.FormatLLM {
		// Display recipients with display-form sender; recipient handles are
		// already user-supplied strings.
		dr := r
		dr.Agent = displayAgent(r.Agent)
		_ = render.EmitLLMHello(os.Stdout, dr)
		return
	}
	out := map[string]any{
		"reserved": true,
		"glob":     r.Glob,
		"intent":   r.Intent,
		"agent":    r.Agent,
		"handle":   r.Handle,
	}
	if len(r.Recipients) > 0 {
		out["to"] = r.Recipients
	}
	_ = render.EmitJSON(os.Stdout, out)
}
