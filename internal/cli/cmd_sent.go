package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"loto/internal/domain"
	"loto/internal/mail"
)

func init() { register("sent", cmdSent) } //nolint:gochecknoinits // command registry pattern

const sentUsage = `usage: loto sent

List your own live outgoing mail with read status — the follow-up view for
"did anyone pick this up?" after a loto msg scrolls out of context. Newest
first; expired mail is omitted.

Read status by address class:
  <uuid>    read=yes once the recipient has read it, else read=no
  @all      read_by=N distinct readers so far
  @<slug>   a repo baton; still listed = not yet consumed (first read deletes it)`

func cmdSent(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, sentUsage) }
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, sentUsage)
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

	msgs, err := box.SentBox(rt.Ctx, rt.agentUUID())
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if len(msgs) == 0 {
		fmt.Fprintln(stdout, "✓ no live sent mail")
		return 0
	}
	fmt.Fprintf(stdout, "ℹ sent count=%d\n", len(msgs))
	for i := range msgs {
		fmt.Fprintln(stdout, sentRow(&msgs[i]))
	}
	return 0
}

// sentRow renders one outgoing message. Broadcast addresses report a reader
// count (read_by); a direct address reports a yes/no since it has one intended
// reader. Body is %q'd like inboxRow — a recipient-less echo of sender-authored
// text still shouldn't smuggle terminal control sequences into Claude's stream.
func sentRow(s *mail.Sent) string {
	m := &s.Msg
	thread := ""
	if m.ThreadID != "" {
		thread = fmt.Sprintf(" thread=%q", m.ThreadID)
	}
	var status string
	if domain.IsBroadcastAddr(m.To) {
		status = fmt.Sprintf("read_by=%d", s.ReadBy)
	} else if s.ReadBy > 0 {
		status = "read=yes"
	} else {
		status = "read=no"
	}
	return fmt.Sprintf("ℹ id=%s to=%s%s %s sent=%s body=%q",
		m.ID, m.To, thread, status, m.CreatedAt.UTC().Format(time.RFC3339), m.Body)
}
