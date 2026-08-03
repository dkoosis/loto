package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
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

// openMailRuntime bootstraps msg/inbox/sent: identity, always; repo context,
// best-effort. mailDBPath never depends on a repo, so unlike openRuntime this
// never hard-fails outside one — RepoTop/Slug are populated when a git
// toplevel resolves (so inbox's @<slug> address matching works exactly as it
// does today inside a repo) and left zero-value otherwise, which mailAddrs
// treats as "no repo address to add" rather than a failure (loto-7wi). A git
// failure that ISN'T "not a repository" (missing binary, timeout, permission)
// gets a warning on stderr rather than silent treatment identical to the
// genuinely-outside-a-repo case (adversarial review on loto-7wi #224).
func openMailRuntime(ctx context.Context, stderr io.Writer) (*runtime, error) {
	agentPinned := identity.PinnedByEnv()
	a, err := identity.Ensure(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	host, _ := os.Hostname()
	sid, pinned := sessionUUID()
	rt := &runtime{
		Agent:         a,
		Ctx:           ctx,
		Host:          host,
		SessionUUID:   domain.SessionUUID(sid),
		SessionPinned: pinned,
		AgentPinned:   agentPinned,
	}
	if top, err := gitRevParseToplevel(ctx); err == nil {
		rt.RepoTop = top
		rt.Slug = ResolveAndPinProjectSlug(top)
	} else if !isNotAGitRepo(err) {
		// A real git/infra failure (missing binary, timeout, permission) inside
		// what may be a live repo — not the "genuinely outside a repo" case.
		// Degrading silently here would drop @<slug>/@<dir> mail with no signal
		// (Codex adversarial review on loto-7wi); warn so the omission is
		// visible instead of just missing mail.
		fmt.Fprintf(stderr, "loto: warning: repo context unavailable (%v); mail scoped to direct/@all only\n", err)
	}
	return rt, nil
}

func (r *runtime) agentUUID() domain.AgentUUID { return domain.AgentUUID(r.Agent.UUID) }

// agentBorn is this reader's identity CreatedAt — the @all cutoff: broadcast
// mail sent before this identity existed never surfaces (loto-mail-lifetimes.1).
func (r *runtime) agentBorn() time.Time { return r.Agent.CreatedAt }

// mailAddrs is this invocation's full address set: direct mail, machine-wide
// broadcast, and the repo addresses of the project being acted in. A repo
// answers to BOTH its pinned slug and its directory basename: senders who
// haven't visited the target repo guess the dir name (@ferret) while the
// pinned slug is remote-derived (@dkoosis-ferret), and send deliberately does
// not validate slugs — so the reader is the only place the alias can resolve.
func (r *runtime) mailAddrs() []string {
	addrs := []string{r.Agent.UUID, domain.AddrAll}
	if r.Slug != "" {
		addrs = append(addrs, domain.RepoAddr(r.Slug))
	}
	if base := filepath.Base(r.RepoTop); base != "" && base != "." && base != string(filepath.Separator) && base != r.Slug {
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
	n, senders, err := box.Summary(r.Ctx, r.agentUUID(), r.agentBorn(), r.mailAddrs())
	if err != nil || n == 0 {
		return
	}
	fmt.Fprintf(w, "ℹ mail unread=%d from=%s — loto inbox\n", n, strings.Join(senders, ","))
}
