package main

import (
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"loto"
	"loto/internal/render"
)

func reserveCmd() *cobra.Command {
	var ttl string

	c := &cobra.Command{
		Use:   "reserve",
		Short: "manage advisory glob reservations",
		Long: `Reserve stakes an advisory claim on a subtree glob pattern.
Reservations do not block acquires but are surfaced as warnings at TryFileLock time.

Subcommands: add, release, list`,
	}

	addCmd := &cobra.Command{
		Use:   "add <glob>",
		Short: "add an advisory reservation for a glob pattern",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := args[0]
			var dur time.Duration
			if ttl != "" {
				dur = parseDurationOrExit("ttl", ttl)
			}
			l := newLOTO()
			overlaps, err := l.OverlappingReservations(pattern)
			if err != nil {
				exit(err)
			}
			r, err := l.Reserve(flagAgent, flagIntent, pattern, dur)
			if err != nil {
				exit(err)
			}
			emitReserveAdded(r, overlaps)
			return nil
		},
	}
	addCmd.Flags().StringVar(&ttl, "ttl", "", "advisory expiry (e.g. 30m, 2h); empty = no expiry")

	releaseCmd := &cobra.Command{
		Use:   "release <glob>",
		Short: "remove an advisory reservation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l := newLOTO()
			if err := l.Unreserve(args[0]); err != nil {
				exit(err)
			}
			emitReserveReleased(args[0])
			return nil
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "list active reservations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			l := newLOTO()
			reservations, err := l.ListReservations()
			if err != nil {
				exit(err)
			}
			if reservations == nil {
				reservations = []*loto.Reservation{}
			}
			emitReserveList(reservations)
			return nil
		},
	}

	c.AddCommand(addCmd, releaseCmd, listCmd)
	return c
}

func reservationEntry(r *loto.Reservation) render.ReservationEntry {
	e := render.ReservationEntry{
		Pattern: r.Pattern,
		AgentID: displayAgent(r.AgentID),
		Intent:  r.Intent,
	}
	if r.ExpiresAt != nil {
		e.ExpiresAt = *r.ExpiresAt
	}
	return e
}

func emitReserveAdded(r *loto.Reservation, overlaps []*loto.Reservation) {
	filtered := make([]*loto.Reservation, 0, len(overlaps))
	for _, o := range overlaps {
		if o.Pattern == r.Pattern && o.AgentID == r.AgentID {
			continue
		}
		filtered = append(filtered, o)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].Pattern < filtered[j].Pattern
	})

	if currentFormat == render.FormatLLM {
		entries := make([]render.ReservationEntry, len(filtered))
		for i, o := range filtered {
			entries[i] = reservationEntry(o)
		}
		_ = render.EmitLLMReserveAdded(os.Stdout, reservationEntry(r), entries)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"reservation": r, "overlaps": filtered})
}

func emitReserveReleased(pattern string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMReserveReleased(os.Stdout, pattern)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{keyReleased: true, "pattern": pattern})
}

func emitReserveList(reservations []*loto.Reservation) {
	if currentFormat == render.FormatLLM {
		entries := make([]render.ReservationEntry, len(reservations))
		for i, r := range reservations {
			entries[i] = reservationEntry(r)
		}
		sort.SliceStable(entries, func(i, j int) bool {
			return entries[i].Pattern < entries[j].Pattern
		})
		_ = render.EmitLLMReserveList(os.Stdout, entries)
		return
	}
	_ = render.EmitJSON(os.Stdout, reservations)
}
