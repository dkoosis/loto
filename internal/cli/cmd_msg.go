package cli

import (
	"flag"
	"fmt"
	"io"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
)

func init() { register("msg", cmdMsg) } //nolint:gochecknoinits // command registry pattern

func cmdMsg(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("msg", flag.ContinueOnError)
	fs.SetOutput(stderr)
	markRead := fs.Bool("mark-read", false, "mark messages as read after listing")
	ttl := fs.Duration("ttl", 0, "optional TTL for sent message (default: never expires)")
	intent := fs.String("t", "", "message body (required when sending)")
	fs.StringVar(intent, "intent", "", "message body (required when sending)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}

	rt, err := openRuntime()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	if fs.NArg() >= 1 {
		return msgSend(rt, fs.Arg(0), *intent, *ttl, stdout, stderr)
	}
	return msgRead(rt, *markRead, stdout, stderr)
}

func msgSend(rt *runtime, toHandle, intent string, ttl time.Duration, stdout, stderr io.Writer) int {
	if intent == "" {
		fmt.Fprintln(stderr, `✗ -t required: loto msg <agent> -t "<text>" [--ttl 4h]`)
		return 2
	}
	ag, err := identity.Resolve(toHandle)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 2
	}
	now := time.Now()
	msg := domain.Message{
		FromUUID:  rt.Agent.UUID,
		ToUUID:    ag.UUID,
		Body:      intent,
		CreatedAt: now,
	}
	if ttl > 0 {
		exp := now.Add(ttl)
		msg.ExpiresAt = &exp
	}
	if err := rt.Store.AddMessage(rt.Ctx, msg); err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	fmt.Fprintf(stdout, "✓ sent to=%s\n", ag.UUID)
	return 0
}

func msgRead(rt *runtime, markRead bool, stdout, stderr io.Writer) int {
	msgs, err := rt.Store.ListUnreadMessages(rt.Ctx, rt.Agent.UUID)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if len(msgs) == 0 {
		fmt.Fprintln(stdout, "✓ no messages")
		return 0
	}
	fmt.Fprintf(stdout, "ℹ messages count=%d\n", len(msgs))
	for i := range msgs {
		m := &msgs[i]
		fmt.Fprintf(stdout, "ℹ id=%s from=%s body=%q sent_at=%s\n",
			m.ID, m.FromUUID, m.Body, m.CreatedAt.UTC().Format(time.RFC3339))
	}
	if markRead {
		_ = rt.Store.MarkMessagesRead(rt.Ctx, rt.Agent.UUID)
	}
	return 0
}
