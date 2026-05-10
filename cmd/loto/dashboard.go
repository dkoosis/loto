package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"loto"
	"loto/internal/render"
)

func dashboardCmd() *cobra.Command {
	var since string
	var agent string

	c := &cobra.Command{
		Use:   "dashboard",
		Short: "live multi-agent activity view (TUI)",
		Long: `Dashboard shows current locks, reservations, recent activity, and
unread mailboxes across all agents. Default is a full-screen TUI for human
oversight; use --format=llm for an event stream consumable by agents.

Press q or ctrl+c to quit.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			sinceDur := parseDurationOrExit("since", since)
			cutoff := time.Time{}
			if sinceDur > 0 {
				cutoff = time.Now().Add(-sinceDur)
			}

			l := newLOTO()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if currentFormat == render.FormatLLM {
				return runDashboardStream(ctx, l, cutoff, agent)
			}
			if !stdoutIsTerminal() {
				return denyDashboardWithoutTerminal()
			}
			return runDashboardTUI(ctx, l, cutoff, agent)
		},
	}
	c.Flags().StringVar(&since, "since", "", "backfill events from now-DURATION (e.g. 5m, 1h)")
	c.Flags().StringVar(&agent, "agent", "", "filter to events involving this agent handle/id")
	return c
}

// ── LLM stream mode ──────────────────────────────────────────────────────────

func runDashboardStream(ctx context.Context, l *loto.LOTO, since time.Time, agent string) error {
	if err := render.EmitLLMDashboardHeader(os.Stdout); err != nil {
		return err
	}
	if !since.IsZero() {
		past, err := l.Backfill(since)
		if err != nil {
			return err
		}
		for _, ev := range past {
			if !matchesAgent(ev, agent) {
				continue
			}
			_ = render.EmitLLMDashboardEvent(os.Stdout, toRenderEvent(ev))
		}
	}
	ch, err := l.Watch(ctx)
	if err != nil {
		return err
	}
	for ev := range ch {
		if !matchesAgent(ev, agent) {
			continue
		}
		_ = render.EmitLLMDashboardEvent(os.Stdout, toRenderEvent(ev))
	}
	return nil
}

// ── TUI mode ─────────────────────────────────────────────────────────────────

func runDashboardTUI(ctx context.Context, l *loto.LOTO, since time.Time, agent string) error {
	ch, err := l.Watch(ctx)
	if err != nil {
		return err
	}
	backfill, _ := l.Backfill(since)
	m := newDashModel(l, ch, backfill, agent)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

const recentMax = 20

type dashModel struct {
	loto      *loto.LOTO
	events    <-chan loto.Event
	agentFilt string
	held      map[string]loto.Event // target → most recent held
	reserved  map[string]loto.Event // pattern → most recent reserved
	recent    []loto.Event
	width     int
	height    int
	now       time.Time
	notice    string
}

func newDashModel(l *loto.LOTO, ch <-chan loto.Event, backfill []loto.Event, agentFilt string) *dashModel {
	m := &dashModel{
		loto:      l,
		events:    ch,
		agentFilt: agentFilt,
		held:      map[string]loto.Event{},
		reserved:  map[string]loto.Event{},
		now:       time.Now(),
	}
	for _, ev := range backfill {
		m.apply(ev)
	}
	return m
}

type evMsg loto.Event
type tickMsg time.Time

func waitForEvent(ch <-chan loto.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return tea.Quit()
		}
		return evMsg(ev)
	}
}

func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *dashModel) Init() tea.Cmd {
	return tea.Batch(waitForEvent(m.events), tickEvery(time.Second))
}

func (m *dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	case evMsg:
		m.apply(loto.Event(msg))
		return m, waitForEvent(m.events)
	case tickMsg:
		m.now = time.Time(msg)
		return m, tickEvery(time.Second)
	}
	return m, nil
}

func (m *dashModel) apply(ev loto.Event) {
	if !matchesAgent(ev, m.agentFilt) {
		return
	}
	switch ev.Kind {
	case loto.EventHeld:
		m.held[ev.Target] = ev
	case loto.EventReleased:
		delete(m.held, ev.Target)
	case loto.EventReserved:
		m.reserved[ev.Target] = ev
	case loto.EventUnreserved:
		delete(m.reserved, ev.Target)
	case loto.EventMsg:
		// msg events only contribute to the recent activity tail
	}
	m.recent = append(m.recent, ev)
	if len(m.recent) > recentMax {
		m.recent = m.recent[len(m.recent)-recentMax:]
	}
}

// ── view ─────────────────────────────────────────────────────────────────────

var (
	styleHeader  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleSection = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14")).MarginTop(1)
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleAgent   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleAge     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleEmpty   = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	styleHelp    = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).MarginTop(1)
)

func (m *dashModel) View() string {
	var b strings.Builder

	title := "loto dashboard"
	if m.agentFilt != "" {
		title += fmt.Sprintf("  (filter: %s)", m.agentFilt)
	}
	b.WriteString(styleHeader.Render(title))
	b.WriteString("  ")
	b.WriteString(styleDim.Render(m.now.Local().Format("15:04:05")))
	b.WriteString("\n")

	b.WriteString(styleSection.Render(fmt.Sprintf("HELD (%d)", len(m.held))))
	b.WriteString("\n")
	if len(m.held) == 0 {
		b.WriteString(styleEmpty.Render("  (no active locks)"))
		b.WriteString("\n")
	} else {
		for _, ev := range sortedByTarget(mapValues(m.held)) {
			fmt.Fprintf(&b, "  %s  %s%s\n",
				styleAgent.Render(padRight(ev.Agent, 16)),
				render.RelPath(ev.Target),
				dimIntent(ev.Intent))
		}
	}

	b.WriteString(styleSection.Render(fmt.Sprintf("RESERVED (%d)", len(m.reserved))))
	b.WriteString("\n")
	if len(m.reserved) == 0 {
		b.WriteString(styleEmpty.Render("  (no reservations)"))
		b.WriteString("\n")
	} else {
		for _, ev := range sortedByTarget(mapValues(m.reserved)) {
			fmt.Fprintf(&b, "  %s  %s%s\n",
				styleAgent.Render(padRight(ev.Agent, 16)),
				ev.Target,
				dimIntent(ev.Intent))
		}
	}

	b.WriteString(styleSection.Render(fmt.Sprintf("ACTIVITY (last %d)", len(m.recent))))
	b.WriteString("\n")
	if len(m.recent) == 0 {
		b.WriteString(styleEmpty.Render("  (none yet — waiting on events…)"))
		b.WriteString("\n")
	} else {
		for _, ev := range slices.Backward(m.recent) {
			b.WriteString("  ")
			b.WriteString(styleAge.Render(humanAge(m.now, ev.Time)))
			b.WriteString("  ")
			b.WriteString(formatActivity(ev))
			b.WriteString("\n")
		}
	}

	b.WriteString(styleHelp.Render("[q] quit"))
	if m.notice != "" {
		b.WriteString("  ")
		b.WriteString(styleHelp.Render(m.notice))
	}
	return b.String()
}

func formatActivity(ev loto.Event) string {
	agent := styleAgent.Render(ev.Agent)
	switch ev.Kind {
	case loto.EventHeld:
		return fmt.Sprintf("%s held %s%s", agent, render.RelPath(ev.Target), dimIntent(ev.Intent))
	case loto.EventReleased:
		return fmt.Sprintf("%s released %s", agent, render.RelPath(ev.Target))
	case loto.EventReserved:
		return fmt.Sprintf("%s reserved %s%s", agent, ev.Target, dimIntent(ev.Intent))
	case loto.EventUnreserved:
		return fmt.Sprintf("%s released-reservation %s", agent, ev.Target)
	case loto.EventMsg:
		body := ev.Body
		if len(body) > 60 {
			body = body[:57] + "…"
		}
		return fmt.Sprintf("%s → %s: %s", agent, ev.To, body)
	}
	return ""
}

func dimIntent(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > 60 {
		s = s[:57] + "…"
	}
	return styleDim.Render("  " + s)
}

func humanAge(now, then time.Time) string {
	d := now.Sub(then)
	switch {
	case d < 0:
		return "now    "
	case d < time.Minute:
		return fmt.Sprintf("%2ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%2dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%2dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%2dd ago", int(d.Hours()/24))
	}
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func mapValues(m map[string]loto.Event) []loto.Event {
	out := make([]loto.Event, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func sortedByTarget(evs []loto.Event) []loto.Event {
	sort.Slice(evs, func(i, j int) bool { return evs[i].Target < evs[j].Target })
	return evs
}

// ── shared helpers ───────────────────────────────────────────────────────────

func matchesAgent(ev loto.Event, agent string) bool {
	if agent == "" {
		return true
	}
	return ev.Agent == agent || ev.To == agent
}

func toRenderEvent(ev loto.Event) render.DashboardEvent {
	return render.DashboardEvent{
		Time:   ev.Time,
		Kind:   string(ev.Kind),
		Agent:  ev.Agent,
		Target: ev.Target,
		Intent: ev.Intent,
		To:     ev.To,
		Body:   ev.Body,
	}
}
