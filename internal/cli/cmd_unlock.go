package cli

import (
	"flag"
	"fmt"
	"io"

	"loto/internal/domain"
)

func init() { register("unlock", cmdUnlock) } //nolint:gochecknoinits // command registry pattern

func cmdUnlock(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("unlock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	allMine := fs.Bool("all-mine", false, "release every lock owned by my uuid")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	rt, err := openRuntime()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	if *allMine {
		return unlockAllMine(rt, stdout, stderr)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: loto unlock <target> | --all-mine")
		return 2
	}
	return unlockSingle(rt, fs.Arg(0), stdout, stderr)
}

func unlockAllMine(rt *runtime, stdout, stderr io.Writer) int {
	all, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	n := 0
	for i := range all {
		if all[i].OwnerUUID != rt.Agent.UUID {
			continue
		}
		if err := rt.Store.ReleaseLock(rt.Ctx, all[i].Target, rt.Agent.UUID); err == nil {
			n++
		}
	}
	fmt.Fprintf(stdout, "✓ released count=%d\n", n)
	return 0
}

func unlockSingle(rt *runtime, arg string, stdout, stderr io.Writer) int {
	t, err := domain.Canonicalize(arg)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 2
	}
	if err := rt.Store.ReleaseLock(rt.Ctx, t, rt.Agent.UUID); err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 1
	}
	tags, _ := rt.Store.TagsOnTarget(rt.Ctx, t)
	for i := range tags {
		tg := &tags[i]
		if tg.AddresseeUUID != "" && tg.AddresseeUUID != rt.Agent.UUID {
			fmt.Fprintf(stdout, "ℹ notify tag=%s addressee=%s\n", tg.ID, tg.AddresseeUUID)
		}
	}
	fmt.Fprintf(stdout, "✓ unlocked target=%s\n", t.Canonical)
	return 0
}
