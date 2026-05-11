package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"loto/internal/domain"
)

func init() { register("inbox", cmdInbox) } //nolint:gochecknoinits // command registry pattern

func cmdInbox(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	unread := fs.Bool("unread", false, "only show unread tags")
	markRead := fs.Bool("mark-read", false, "advance read cursor for each target after listing")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	rt, err := openRuntime()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	all, err := rt.Store.ListAllTags(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	mine := filterAddresseeTags(all, rt.Agent.UUID)
	if *unread {
		mine = filterUnreadTags(rt, mine)
	}
	sortInboxTags(mine)
	printInboxTags(stdout, mine)

	if *markRead {
		markInboxRead(rt, mine)
	}
	return 0
}

func filterAddresseeTags(all []domain.TagRecord, agentUUID string) []domain.TagRecord {
	mine := make([]domain.TagRecord, 0, len(all))
	for i := range all {
		if all[i].AddresseeUUID == agentUUID {
			mine = append(mine, all[i])
		}
	}
	return mine
}

func filterUnreadTags(rt *runtime, mine []domain.TagRecord) []domain.TagRecord {
	byTarget := map[string][]domain.TagRecord{}
	order := []string{}
	for i := range mine {
		c := mine[i].Target.Canonical
		if _, ok := byTarget[c]; !ok {
			order = append(order, c)
		}
		byTarget[c] = append(byTarget[c], mine[i])
	}
	sort.Strings(order)
	var filtered []domain.TagRecord
	for _, c := range order {
		t := domain.Target{Canonical: c, Kind: byTarget[c][0].Target.Kind}
		unr, err := rt.Store.UnreadTagsForAddressee(rt.Ctx, rt.Agent.UUID, t)
		if err != nil {
			continue
		}
		filtered = append(filtered, unr...)
	}
	return filtered
}

func sortInboxTags(mine []domain.TagRecord) {
	sort.Slice(mine, func(i, j int) bool {
		if mine[i].Target.Canonical != mine[j].Target.Canonical {
			return mine[i].Target.Canonical < mine[j].Target.Canonical
		}
		if !mine[i].CreatedAt.Equal(mine[j].CreatedAt) {
			return mine[i].CreatedAt.Before(mine[j].CreatedAt)
		}
		return mine[i].ID < mine[j].ID
	})
}

func printInboxTags(stdout io.Writer, mine []domain.TagRecord) {
	if len(mine) == 0 {
		fmt.Fprintln(stdout, "✓ no unread")
		return
	}
	fmt.Fprintf(stdout, "ℹ tags count=%d\n", len(mine))
	for i := range mine {
		tg := &mine[i]
		fmt.Fprintf(stdout, "ℹ target=%s tag=%s author=%s intent=%q created_at=%s\n",
			tg.Target.Canonical, tg.ID, tg.AuthorUUID, tg.Intent,
			tg.CreatedAt.UTC().Format(time.RFC3339))
	}
}

func markInboxRead(rt *runtime, mine []domain.TagRecord) {
	// Advance the cursor only as far as the latest tag we actually showed
	// per target — see gh#47. Querying MAX(created_at) inside the store
	// would race with concurrent AddTag calls landing between display and
	// MarkRead.
	maxByTarget := map[string]time.Time{}
	targets := map[string]domain.Target{}
	for i := range mine {
		c := mine[i].Target.Canonical
		targets[c] = mine[i].Target
		if mine[i].CreatedAt.After(maxByTarget[c]) {
			maxByTarget[c] = mine[i].CreatedAt
		}
	}
	for c, t := range targets {
		_ = rt.Store.MarkRead(rt.Ctx, rt.Agent.UUID, t, maxByTarget[c])
	}
}
