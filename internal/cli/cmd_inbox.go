package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/mail"
)

func init() { register("inbox", cmdInbox) } //nolint:gochecknoinits // command registry pattern

const inboxUsage = `usage: loto inbox [--mark-read] [--summary]

List unread mail for this agent: direct, @all, and this repo's addresses
(pinned slug and directory basename). @all mail sent before this session's
identity existed is filtered out — broadcasts reach whoever is working now.

  --mark-read   consume everything listed. Direct/@all mail is stamped read
                per-reader (stays unread for other agents); @<slug> repo mail
                is a baton — the first reader deletes it for everyone.
  --summary     one-line unread count for hooks/banners; silent when empty`

func cmdInbox(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, inboxUsage) }
	markRead := fs.Bool("mark-read", false, "mark listed messages as read")
	summary := fs.Bool("summary", false, "one-line unread summary; silent when empty")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, inboxUsage)
		return 2
	}
	rt, err := openRuntime(ctx)
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

	if *summary {
		n, senders, err := box.Summary(rt.Ctx, rt.agentUUID(), rt.agentBorn(), rt.mailAddrs())
		if err != nil || n == 0 {
			return 0 // silent: the summary surface is a banner, not a report
		}
		fmt.Fprintf(stdout, "ℹ mail unread=%d from=%s — loto inbox\n", n, strings.Join(senders, ","))
		return 0
	}
	return inboxList(rt, box, *markRead, stdout, stderr)
}

func inboxList(rt *runtime, box *mail.Box, markRead bool, stdout, stderr io.Writer) int {
	msgs, err := box.Inbox(rt.Ctx, rt.agentUUID(), rt.agentBorn(), rt.mailAddrs())
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if len(msgs) == 0 {
		fmt.Fprintln(stdout, "✓ inbox empty")
		return 0
	}
	fmt.Fprintf(stdout, "ℹ unread count=%d\n", len(msgs))
	ids := make([]string, len(msgs))
	for i := range msgs {
		ids[i] = msgs[i].ID
		fmt.Fprintln(stdout, inboxRow(&msgs[i]))
	}
	if markRead {
		// Mark exactly the IDs displayed above — not a fresh address re-query,
		// which would race a concurrent Send and mark an unshown message read.
		if err := box.MarkRead(rt.Ctx, rt.agentUUID(), ids); err != nil {
			fmt.Fprintf(stderr, "✗ mark-read: %v\n", err)
			return 3
		}
		fmt.Fprintf(stdout, "✓ marked read count=%d\n", len(msgs))
	}
	return 0
}

func inboxRow(m *domain.Message) string {
	from := m.FromHandle
	if from == "" {
		from = string(m.FromUUID)
		if len(from) > 8 {
			from = from[:8]
		}
	}
	// %q the sender-controlled thread id, as with the body: an agent could
	// otherwise smuggle ANSI/OSC terminal control sequences into this
	// Claude-consumed output (loto-qhw, CodeRabbit).
	thread := ""
	if m.ThreadID != "" {
		thread = fmt.Sprintf(" thread=%q", m.ThreadID)
	}
	return fmt.Sprintf("ℹ id=%s from=%s to=%s%s sent=%s body=%q",
		m.ID, from, m.To, thread, m.CreatedAt.UTC().Format(time.RFC3339), m.Body)
}
