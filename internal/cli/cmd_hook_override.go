package cli

import (
	"context"
	"fmt"
	"io"

	"loto/internal/identity"
)

// ── `loto hook override` — the row LOTO_GUARD_OVERRIDE=1 owes (loto-mh07) ──
//
// The guards in .githooks/hooks.d/ read LOTO_GUARD_OVERRIDE=1 and skip
// themselves without ever calling loto — a bypass that leaves no trace, so an
// override spike could not be counted. This subcommand is what each guard
// calls FROM its override branch, in place of the silent skip: one event,
// naming the guard, before the guard exits 0 exactly as it did before.

// hookOverrideGuardPostCheckout and hookOverrideGuardReferenceTransaction are
// the only guard names this command accepts — spelled the way the hooks.d
// directory names them, so the events table reads the same vocabulary as the
// tree it audits. pre-commit left with the staged-lock gate (loto-ojq3).
const (
	hookOverrideGuardPostCheckout         = "post-checkout"
	hookOverrideGuardReferenceTransaction = "reference-transaction"
)

// cmdHookOverride is the `override` arm of the hook router in cmd_hook.go.
//
// ‡ Every path below returns 0 except a usage error. The call sites are
// shell guards whose whole point, once LOTO_GUARD_OVERRIDE=1 is set, is
// letting the git operation through no matter what; a runtime that cannot
// open the store, or a write that fails once it is open, is reported on
// stderr and otherwise ignored rather than propagated as a nonzero exit —
// recording the override must never become a second way to block it.
func cmdHookOverride(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, hookUsageHead)
		return 2
	}
	guard := args[0]
	if guard == subHelp || guard == "-h" || guard == flagHelpLong {
		fmt.Fprint(stdout, hookUsageHead)
		return 0
	}
	switch guard {
	case hookOverrideGuardPostCheckout, hookOverrideGuardReferenceTransaction:
	default:
		fmt.Fprintf(stderr, "✗ unknown guard %q\n", guard)
		fmt.Fprint(stderr, hookUsageHead)
		return 2
	}

	// Checked before the runtime opens, same as runHook (cmd_hook.go): an
	// unpinned caller — a human running git under the override, or any
	// process with nothing in the environment naming an identity — gets a
	// throwaway owner from identity.Ephemeral if this proceeds, and a
	// guard_override row attributed to a UUID that exists for one process
	// pollutes the exact signal this event kind exists to give a promotion
	// (loto-mh07 review). Skip and say so instead.
	if !identity.PinnedByEnv() {
		return hookSkip(stderr, "%v", errIdentityUnpinned)
	}

	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ guard-override=unrecorded guard=%s err=%q\n", guard, err)
		return 0
	}
	defer rt.Close()

	if err := rt.Store.RecordGuardOverride(rt.Ctx, rt.Agent.UUID, guard); err != nil {
		fmt.Fprintf(stderr, "⚠ guard-override=unrecorded guard=%s err=%q\n", guard, err)
	}
	return 0
}
