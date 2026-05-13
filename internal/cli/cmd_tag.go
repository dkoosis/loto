package cli

import (
	"flag"
	"fmt"
	"io"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
)

func init() { //nolint:gochecknoinits // command registry pattern
	register("tag", cmdTag)
	register("untag", cmdUntag)
}

func cmdTag(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tag", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "", "addressee handle or uuid")
	ttl := fs.Duration("ttl", 0, "optional TTL (default: never expires)")
	intent := fs.String("t", "", "note (required)")
	fs.StringVar(intent, "intent", "", "note (required)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if *intent == "" {
		fmt.Fprintln(stderr, `✗ -t required: loto tag <target> [--to <agent>] [--ttl 1h] -t "<note>"`)
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, `usage: loto tag <target> [--to <agent>] [--ttl 1h] -t "<note>"`)
		return 2
	}
	target, err := domain.Canonicalize(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 2
	}
	rt, err := openRuntime()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	return tagApply(rt, target, *to, *intent, *ttl, stdout, stderr)
}

func tagApply(rt *runtime, target domain.Target, toHandle, intent string, ttl time.Duration, stdout, stderr io.Writer) int {
	addressee := ""
	if toHandle != "" {
		ag, err := identity.Resolve(toHandle)
		if err != nil {
			fmt.Fprintf(stderr, "✗ %v\n", err)
			return 2
		}
		addressee = ag.UUID
	}
	now := time.Now()
	tg := domain.TagRecord{
		Target: target, Kind: domain.TagNote,
		AuthorUUID: rt.Agent.UUID, AddresseeUUID: addressee, Intent: intent,
		CreatedAt: now,
	}
	if ttl > 0 {
		exp := now.Add(ttl)
		tg.ExpiresAt = &exp
	}
	id, err := rt.Store.AddTag(rt.Ctx, tg)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	fmt.Fprintf(stdout, "✓ tagged target=%s tag=%s\n", target.Canonical, id)
	return 0
}

func cmdUntag(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("untag", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: loto untag <target> <tag-id>")
		return 2
	}
	target, err := domain.Canonicalize(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 2
	}
	rt, err := openRuntime()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	if err := rt.Store.RemoveTag(rt.Ctx, target, fs.Arg(1), rt.Agent.UUID); err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "✓ removed target=%s tag=%s\n", target.Canonical, fs.Arg(1))
	return 0
}

// cmdAnnotate is the bare-form handler: loto <target> -t "note"
func cmdAnnotate(args []string, stdout, stderr io.Writer) int {
	return cmdTag(args, stdout, stderr)
}
