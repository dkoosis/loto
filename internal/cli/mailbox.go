package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"loto/internal/domain"
	"loto/internal/mail"
)

// mailDBPath returns the host-global mail DB location:
// $XDG_STATE_HOME/loto/mail.db — deliberately OUTSIDE projects/<slug>/ so one
// mailbox spans every repo on the machine. LOTO_BASE relocates it like all
// other state (which also scopes mail per-test in the acceptance harness).
func mailDBPath() string {
	if v := os.Getenv("LOTO_BASE"); v != "" {
		return filepath.Join(v, "mail.db")
	}
	return filepath.Join(xdgStateHome(), "loto", "mail.db")
}

// OpenMail opens the host-global mailbox. Callers own Close.
func (r *runtime) OpenMail() (*mail.Box, error) {
	return mail.Open(r.Ctx, mailDBPath())
}

func (r *runtime) agentUUID() domain.AgentUUID { return domain.AgentUUID(r.Agent.UUID) }

// mailAddrs is this invocation's full address set: direct mail, machine-wide
// broadcast, and the repo addresses of the project being acted in. A repo
// answers to BOTH its pinned slug and its directory basename: senders who
// haven't visited the target repo guess the dir name (@ferret) while the
// pinned slug is remote-derived (@dkoosis-ferret), and send deliberately does
// not validate slugs — so the reader is the only place the alias can resolve.
func (r *runtime) mailAddrs() []string {
	addrs := []string{r.Agent.UUID, domain.AddrAll, domain.RepoAddr(r.Slug)}
	if base := filepath.Base(r.RepoTop); base != "" && base != "." && base != r.Slug {
		addrs = append(addrs, domain.RepoAddr(base))
	}
	return addrs
}

// DeferredMailFooter prints a one-line unread banner after the primary command
// output — the push half of the mailbox: agents see mail because a command
// they already run surfaced it, not because they remembered to poll. Same
// opt-in set as DeferredTagFooter; check stays excluded (pinned machine
// surface). Best-effort throughout: mail is a channel, not an invariant, so
// any error renders as silence.
func (r *runtime) DeferredMailFooter(w io.Writer) {
	box, err := r.OpenMail()
	if err != nil {
		return
	}
	defer box.Close()
	n, senders, err := box.Summary(r.Ctx, r.agentUUID(), r.mailAddrs())
	if err != nil || n == 0 {
		return
	}
	fmt.Fprintf(w, "ℹ mail unread=%d from=%s — loto inbox\n", n, strings.Join(senders, ","))
}
