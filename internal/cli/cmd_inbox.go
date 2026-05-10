package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"loto/internal/domain"
)

func init() { register("inbox", cmdInbox) }

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
	now := time.Now()

	// Filter: addressed to me. For --unread, also strip tags older than the cursor.
	mine := make([]domain.TagRecord, 0, len(all))
	for _, tg := range all {
		if tg.AddresseeUUID != rt.Agent.UUID {
			continue
		}
		mine = append(mine, tg)
	}

	if *unread {
		// Group by target, query cursor per target.
		byTarget := map[string][]domain.TagRecord{}
		order := []string{}
		for _, tg := range mine {
			c := tg.Target.Canonical
			if _, ok := byTarget[c]; !ok {
				order = append(order, c)
			}
			byTarget[c] = append(byTarget[c], tg)
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
		mine = filtered
	}

	sort.Slice(mine, func(i, j int) bool {
		if mine[i].Target.Canonical != mine[j].Target.Canonical {
			return mine[i].Target.Canonical < mine[j].Target.Canonical
		}
		if !mine[i].CreatedAt.Equal(mine[j].CreatedAt) {
			return mine[i].CreatedAt.Before(mine[j].CreatedAt)
		}
		return mine[i].ID < mine[j].ID
	})

	if len(mine) == 0 {
		fmt.Fprintln(stdout, "✓ no unread")
	} else {
		fmt.Fprintf(stdout, "ℹ tags count=%d\n", len(mine))
		for _, tg := range mine {
			fmt.Fprintf(stdout, "ℹ target=%s tag=%s author=%s intent=%q created_at=%s\n",
				tg.Target.Canonical, tg.ID, tg.AuthorUUID, tg.Intent,
				tg.CreatedAt.UTC().Format(time.RFC3339))
		}
	}

	if *markRead {
		seen := map[string]domain.Target{}
		for _, tg := range mine {
			seen[tg.Target.Canonical] = tg.Target
		}
		for _, t := range seen {
			_ = rt.Store.MarkRead(rt.Ctx, rt.Agent.UUID, t)
		}
	}
	_ = now
	return 0
}
