package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"loto"
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
				d, err := time.ParseDuration(ttl)
				if err != nil {
					fmt.Fprintf(os.Stderr, "loto: invalid --ttl %q: %v\n", ttl, err)
					os.Exit(2)
				}
				dur = d
			}
			l := newLOTO()
			r, err := l.Reserve(flagAgent, flagIntent, pattern, dur)
			if err != nil {
				exit(err)
			}
			printJSON(r)
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
			printJSON(map[string]any{"released": true, "pattern": args[0]})
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
			printJSON(reservations)
			return nil
		},
	}

	c.AddCommand(addCmd, releaseCmd, listCmd)
	return c
}
