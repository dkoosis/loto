package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
)

// errEmptyAtAddr rejects a bare "@" recipient token.
var errEmptyAtAddr = errors.New("empty @-address")

func init() { register("msg", cmdMsg) } //nolint:gochecknoinits // command registry pattern

// msgUsage is the point-of-use teaching surface (same policy as tagUsage):
// Claude is loto's primary user, so the input contract lives in the binary.
const msgUsage = `usage: loto msg <to> -t "<text>" [--ttl 168h] [--thread <id>]

Send store-and-forward mail to another agent. Host-global: crosses repos.
The address class sets the message lifetime:

  <handle>   a specific agent (e.g. SunnyPorcupine) — read per-reader; 7d TTL
  @<slug>    whoever NEXT acts in that repo — pinned slug or its directory
             basename both deliver (e.g. @dkoosis-ferret or @ferret). A baton:
             the first reader consumes it for everyone; 30d TTL
  @all       every agent working NOW — invisible to sessions started after the
             send; read per-reader; 7d TTL

Convention: open the text with your bead id, then the ask.
Default TTL 168h direct/@all, 720h repo; --ttl overrides. Unread mail expires
silently.

examples:
  loto msg @loto -t "loto-qhw: does check --gate handle claims?"
  loto msg SunnyPorcupine -t "loto-qhw: ei5 done? I need session release"`

func cmdMsg(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("msg", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, msgUsage) }
	text := fs.String("t", "", "message body (required)")
	fs.StringVar(text, "intent", "", "message body (required)")
	ttl := fs.Duration("ttl", 0, "message TTL (default 168h)")
	thread := fs.String("thread", "", "optional thread id grouping replies")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 || strings.TrimSpace(*text) == "" {
		fmt.Fprintln(stderr, msgUsage)
		return 2
	}
	// Reject a negative TTL rather than silently applying the 7d default: a
	// caller writing --ttl=-1h means "expire fast", and falling through to the
	// default retains mail they meant to drop (loto-qhw, Codex/CodeRabbit).
	// 0 stays valid — it selects the default, matching lock/claim.
	if *ttl < 0 {
		fmt.Fprintf(stderr, "✗ --ttl must be non-negative, got %s\n", *ttl)
		return 2
	}
	warnIfNoBeadID(*text, stderr)
	to, err := resolveMsgAddr(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 2
	}
	return msgSend(ctx, to, *text, *thread, *ttl, stdout, stderr)
}

func msgSend(ctx context.Context, to, body, thread string, ttl time.Duration, stdout, stderr io.Writer) int {
	rt, err := openMailRuntime(ctx, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	box, err := rt.OpenMail()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer box.Close()
	m := domain.Message{
		FromUUID:   domain.AgentUUID(rt.Agent.UUID),
		FromHandle: rt.Agent.Handle,
		To:         to,
		Body:       body,
		ThreadID:   thread,
	}
	if ttl > 0 {
		now := time.Now()
		m.CreatedAt = now
		m.ExpiresAt = now.Add(ttl)
	}
	id, err := box.Send(rt.Ctx, m)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	fmt.Fprintf(stdout, "✓ sent id=%s to=%s\n", id, to)
	return 0
}

// resolveMsgAddr maps the CLI <to> token to a stored address: @-addresses pass
// through verbatim (repo slugs are not validated — the target repo may not
// have been visited yet on this machine), anything else must resolve to a
// registered agent handle or UUID.
func resolveMsgAddr(raw string) (string, error) {
	if strings.HasPrefix(raw, "@") {
		if len(raw) == 1 {
			return "", errEmptyAtAddr
		}
		return raw, nil
	}
	if a, err := identity.LookupByUUID(raw); err == nil {
		return a.UUID, nil
	}
	a, err := identity.LookupByHandle(raw)
	if err != nil {
		return "", err
	}
	return a.UUID, nil
}
