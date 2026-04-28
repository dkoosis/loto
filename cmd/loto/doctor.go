package main

import (
	"os"

	"github.com/spf13/cobra"
	"loto"
)

func doctorCmd() *cobra.Command {
	var dryRun bool
	var repair bool

	c := &cobra.Command{
		Use:   "doctor",
		Short: "diagnose and optionally repair stale coordination state",
		Long: `Doctor walks the coordination base and detects five drift classes:

  stale_tag       tag present, flock unheld (crash remnant)
  dead_pid        tag's PID is dead regardless of flock state
  orphaned        unpaired lock/tag or target path gone from disk
  layout_drift    unexpected files in coordination base (report only)
  soft_stale_held soft TTL expired but lock still held (report only)

Exit codes: 0 = clean, 1 = drift found, 3 = system error.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := loto.DoctorCheck
			if repair {
				mode = loto.DoctorRepair
			} else if dryRun {
				mode = loto.DoctorDryRun
			}
			l := newLOTO()
			report, err := l.Doctor(flagAgent, mode)
			if err != nil {
				exit(err)
			}
			printJSON(report)
			if !report.Clean {
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show what --repair would do without making changes")
	c.Flags().BoolVar(&repair, "repair", false, "apply safe repairs (remove stale tags, force-break dead-PID holders)")
	return c
}
