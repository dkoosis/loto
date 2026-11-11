// Package gorules is a ruleguard bundle: machine-checkable rules for the
// recurring Go defect classes surfaced by the 9-repo cross-repo scan
// (decision nug d7ea466a8172). Each rule traces to real, verbatim-repeated
// defects and runs inside golangci-lint's gocritic ruleguard checker. See
// README.md for enablement, per-rule provenance, and the escape hatch.
//
// This file is DSL, not a program: ruleguard parses it, `go build` never
// compiles it. It still lives in a real module so editor tooling and the
// analysistest harness (rules_test.go) work.
//
// Rule coverage lives across three surfaces because no single golangci
// mechanism spans them:
//   - ruleguard (this file): scanner-buffer, raw-state-write, err-substring,
//     pid-liveness, exec-no-wait-delay.
//   - forbidigo (see golangci.snippet.yml): context.Background in daemons.
//   - a standalone analyzer (../omitemptyscalar): omitempty on meaningful
//     zero-value fields — ruleguard can't read struct tags (README §rule 3).
//
// A consuming repo copies this file to .golangci-rules/bugclasses.go verbatim.
// That copy drifts silently when the pack changes, so the pack is versioned:
// bump the token below on ANY rule edit; repo-standard embeds the pack and its
// lint-pack rule compares each repo's copy byte for byte (repair: `repo-standard
// pack sync`).
//
//pack:version v3
package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// Bundle is the ruleguard bundle handle. It exists for the standalone
// `ruleguard` binary, which can dsl.ImportRules() this pack from the module
// cache and skip the file copy.
//
// It does NOT work under golangci-lint (the surface every consuming repo
// actually runs). golangci-lint's embedded gocritic/ruleguard typechecks rule
// files with a GOPATH-only source importer — it ignores go.mod, replace
// directives, the module cache, and vendor/. A downstream file that imports
// this package by module path fails ruleguard init with "cannot find package
// ... (from $GOROOT)/(from $GOPATH)". Verified against golangci-lint v2.12.2 /
// ruleguard dsl v0.3.22 (spike, 2026-08-04). So consuming repos MUST copy the
// rule file and keep the copy byte-equal (repo-standard's lint-pack rule). Do not wire an
// ImportRules shim expecting golangci-lint to resolve it.
var Bundle = dsl.Bundle{}

// --- Rule 1: bufio.Scanner with the default 64KB token cap ------------------

//doc:summary bufio.NewScanner consumed without a Buffer() cap override
//doc:before  sc := bufio.NewScanner(r); for sc.Scan() { ... }
//doc:after   sc := bufio.NewScanner(r); sc.Buffer(buf, max); for sc.Scan() { ... }
//doc:tags    correctness
func scannerDefaultBuffer(m dsl.Matcher) {
	// The defect shape: a scanner is created and consumed with the scan loop
	// adjacent to the constructor. A Buffer() call sits BETWEEN the two lines
	// in fixed code, breaking statement adjacency, so this stays silent once
	// the cap is set. Heuristic, not flow analysis — README §rule 1.
	m.Match(
		`$sc := bufio.NewScanner($r)
		 for $sc.Scan() { $*_ }`,
	).
		// Skip tests (fixtures aren't production input). An in-memory reader
		// is not exempt: one line over 64KB in a loaded value still stops
		// Scan with bufio.ErrTooLong.
		Where(!m.File().Name.Matches(`_test\.go$`)).
		Report(`bufio.Scanner default 64KB token cap truncates long input; call $sc.Buffer(buf, max)`)
}

// Rule 2 (raw durable-state write) lives in optin_rawstatewrite.go — it ships
// OFF, so it is a separate file the `rules` glob includes only once a repo
// adopts atomicfile. gocritic's ruleguard checker exposes no per-group
// disable, so file selection is the toggle (README §enablement).

// --- Rule 4: error classification by substring ------------------------------

//doc:summary error classified by matching err.Error() text
//doc:before  strings.Contains(err.Error(), "not found")
//doc:after   errors.Is(err, ErrNotFound)
//doc:tags    correctness
func errSubstringMatch(m dsl.Matcher) {
	// strings.Contains / HasPrefix / HasSuffix / EqualFold over an .Error()
	// string. Test files legitimately assert on error text; the defect is
	// production control flow keyed on a message. Exclude _test.go.
	m.Match(
		`strings.Contains($err.Error(), $_)`,
		`strings.HasPrefix($err.Error(), $_)`,
		`strings.HasSuffix($err.Error(), $_)`,
		`strings.EqualFold($err.Error(), $_)`,
	).
		Where(m["err"].Type.Implements(`error`) &&
			!m.File().Name.Matches(`_test\.go$`)).
		Report(`error classified by substring; use typed sentinels + errors.Is/As`)

	// The mirror shape: the error's text is the NEEDLE, a stored message the
	// haystack (canapay ebe3e74: strings.Contains(m.Detail, errX.Error())).
	// Same defect: control flow keyed on message text, not on the error.
	m.Match(
		`strings.Contains($_, $e.Error())`,
		`strings.HasPrefix($_, $e.Error())`,
		`strings.HasSuffix($_, $e.Error())`,
		`strings.EqualFold($_, $e.Error())`,
	).
		Where(m["e"].Type.Implements(`error`) &&
			!m.File().Name.Matches(`_test\.go$`)).
		Report(`error text used as a needle; record a typed field or sentinel and use errors.Is/As`)

	// Direct equality/inequality of an error's message to a constant.
	m.Match(
		`$err.Error() == $s`,
		`$err.Error() != $s`,
		`$s == $err.Error()`,
		`$s != $err.Error()`,
	).
		Where(m["err"].Type.Implements(`error`) && m["s"].Const &&
			!m.File().Name.Matches(`_test\.go$`)).
		Report(`error compared by message text; use errors.Is with a sentinel`)

	// Two errors compared by their text (sdlc: fail == errLeft.Error()). An
	// error's text against an arbitrary string variable is NOT matched: the
	// measured hits (canapay vendorconnect `msg != err.Error()`) were output
	// de-duplication, not classification.
	m.Match(
		`$a.Error() == $b.Error()`,
		`$a.Error() != $b.Error()`,
	).
		Where(m["a"].Type.Implements(`error`) && m["b"].Type.Implements(`error`) &&
			!m.File().Name.Matches(`_test\.go$`)).
		Report(`error compared by message text; use errors.Is with a sentinel`)
}

// --- Rule 6: PID-only liveness check ----------------------------------------

//doc:summary liveness probed by signalling a PID with no start-time witness
//doc:before  proc.Signal(syscall.Signal(0))
//doc:after   witness process start-time (PID reuse gives a false positive)
//doc:tags    correctness
func pidLivenessSignal(m dsl.Matcher) {
	// Signal(0) on an *os.Process is the textbook PID-liveness probe; it says
	// nothing about PID reuse. Narrow — the zero-signal idiom is rare outside
	// this check, so it does not fire on real signalling.
	m.Match(
		`$p.Signal(syscall.Signal(0))`,
	).
		Report(`PID-only liveness: reused PID gives a false positive; witness proc start-time`)
}

// --- Rule 7: subprocess with no WaitDelay -----------------------------------

//doc:summary exec.CommandContext run with no cmd.WaitDelay
//doc:before  out, err := exec.CommandContext(ctx, "git", "log").Output()
//doc:after   cmd := exec.CommandContext(ctx, "git", "log"); cmd.WaitDelay = 5 * time.Second; out, err := cmd.Output()
//doc:tags    correctness
func execNoWaitDelay(m dsl.Matcher) {
	// The defect: after ctx is cancelled the child is killed, but a grandchild
	// holding the stdout/stderr pipe keeps Wait blocked forever unless
	// cmd.WaitDelay is set (12 fixes in 6 repos). ruleguard cannot prove an
	// absence by flow, so only two precise shapes match: the chained form,
	// where no WaitDelay can have been set, and the constructor immediately
	// followed by the run, the adjacency heuristic rule 1 uses. A WaitDelay
	// assignment sits BETWEEN the two lines in fixed code and breaks adjacency.
	// The run may be a bare call, an assignment, `return`, or the init of an
	// `if err := cmd.Run(); err != nil`. A command built here and run
	// elsewhere is invisible (README rule 7).
	m.Match(
		`exec.CommandContext($*_).Output()`,
		`exec.CommandContext($*_).CombinedOutput()`,
		`exec.CommandContext($*_).Run()`,
		`exec.CommandContext($*_).Start()`,
		`$c := exec.CommandContext($*_)
		 $c.Output()`,
		`$c := exec.CommandContext($*_)
		 $_, $_ := $c.Output()`,
		`$c := exec.CommandContext($*_)
		 $_, $_ = $c.Output()`,
		`$c := exec.CommandContext($*_)
		 $_ := $c.Output()`,
		`$c := exec.CommandContext($*_)
		 if $_, $_ := $c.Output(); $_ { $*_ }`,
		`$c := exec.CommandContext($*_)
		 return $c.Output()`,
		`$c := exec.CommandContext($*_)
		 $c.CombinedOutput()`,
		`$c := exec.CommandContext($*_)
		 $_, $_ := $c.CombinedOutput()`,
		`$c := exec.CommandContext($*_)
		 $_, $_ = $c.CombinedOutput()`,
		`$c := exec.CommandContext($*_)
		 $_ := $c.CombinedOutput()`,
		`$c := exec.CommandContext($*_)
		 if $_, $_ := $c.CombinedOutput(); $_ { $*_ }`,
		`$c := exec.CommandContext($*_)
		 return $c.CombinedOutput()`,
		`$c := exec.CommandContext($*_)
		 $c.Run()`,
		`$c := exec.CommandContext($*_)
		 $_ := $c.Run()`,
		`$c := exec.CommandContext($*_)
		 $_ = $c.Run()`,
		`$c := exec.CommandContext($*_)
		 if $_ := $c.Run(); $_ { $*_ }`,
		`$c := exec.CommandContext($*_)
		 return $c.Run()`,
		`$c := exec.CommandContext($*_)
		 $c.Start()`,
		`$c := exec.CommandContext($*_)
		 $_ := $c.Start()`,
		`$c := exec.CommandContext($*_)
		 $_ = $c.Start()`,
		`$c := exec.CommandContext($*_)
		 if $_ := $c.Start(); $_ { $*_ }`,
		`$c := exec.CommandContext($*_)
		 return $c.Start()`,
		`$c = exec.CommandContext($*_)
		 $c.Output()`,
		`$c = exec.CommandContext($*_)
		 $_, $_ := $c.Output()`,
		`$c = exec.CommandContext($*_)
		 $_, $_ = $c.Output()`,
		`$c = exec.CommandContext($*_)
		 $_ := $c.Output()`,
		`$c = exec.CommandContext($*_)
		 if $_, $_ := $c.Output(); $_ { $*_ }`,
		`$c = exec.CommandContext($*_)
		 return $c.Output()`,
		`$c = exec.CommandContext($*_)
		 $c.CombinedOutput()`,
		`$c = exec.CommandContext($*_)
		 $_, $_ := $c.CombinedOutput()`,
		`$c = exec.CommandContext($*_)
		 $_, $_ = $c.CombinedOutput()`,
		`$c = exec.CommandContext($*_)
		 $_ := $c.CombinedOutput()`,
		`$c = exec.CommandContext($*_)
		 if $_, $_ := $c.CombinedOutput(); $_ { $*_ }`,
		`$c = exec.CommandContext($*_)
		 return $c.CombinedOutput()`,
		`$c = exec.CommandContext($*_)
		 $c.Run()`,
		`$c = exec.CommandContext($*_)
		 $_ := $c.Run()`,
		`$c = exec.CommandContext($*_)
		 $_ = $c.Run()`,
		`$c = exec.CommandContext($*_)
		 if $_ := $c.Run(); $_ { $*_ }`,
		`$c = exec.CommandContext($*_)
		 return $c.Run()`,
		`$c = exec.CommandContext($*_)
		 $c.Start()`,
		`$c = exec.CommandContext($*_)
		 $_ := $c.Start()`,
		`$c = exec.CommandContext($*_)
		 $_ = $c.Start()`,
		`$c = exec.CommandContext($*_)
		 if $_ := $c.Start(); $_ { $*_ }`,
		`$c = exec.CommandContext($*_)
		 return $c.Start()`,
		`var $c = exec.CommandContext($*_)
		 $c.Output()`,
		`var $c = exec.CommandContext($*_)
		 $_, $_ := $c.Output()`,
		`var $c = exec.CommandContext($*_)
		 $_, $_ = $c.Output()`,
		`var $c = exec.CommandContext($*_)
		 $_ := $c.Output()`,
		`var $c = exec.CommandContext($*_)
		 if $_, $_ := $c.Output(); $_ { $*_ }`,
		`var $c = exec.CommandContext($*_)
		 return $c.Output()`,
		`var $c = exec.CommandContext($*_)
		 $c.CombinedOutput()`,
		`var $c = exec.CommandContext($*_)
		 $_, $_ := $c.CombinedOutput()`,
		`var $c = exec.CommandContext($*_)
		 $_, $_ = $c.CombinedOutput()`,
		`var $c = exec.CommandContext($*_)
		 $_ := $c.CombinedOutput()`,
		`var $c = exec.CommandContext($*_)
		 if $_, $_ := $c.CombinedOutput(); $_ { $*_ }`,
		`var $c = exec.CommandContext($*_)
		 return $c.CombinedOutput()`,
		`var $c = exec.CommandContext($*_)
		 $c.Run()`,
		`var $c = exec.CommandContext($*_)
		 $_ := $c.Run()`,
		`var $c = exec.CommandContext($*_)
		 $_ = $c.Run()`,
		`var $c = exec.CommandContext($*_)
		 if $_ := $c.Run(); $_ { $*_ }`,
		`var $c = exec.CommandContext($*_)
		 return $c.Run()`,
		`var $c = exec.CommandContext($*_)
		 $c.Start()`,
		`var $c = exec.CommandContext($*_)
		 $_ := $c.Start()`,
		`var $c = exec.CommandContext($*_)
		 $_ = $c.Start()`,
		`var $c = exec.CommandContext($*_)
		 if $_ := $c.Start(); $_ { $*_ }`,
		`var $c = exec.CommandContext($*_)
		 return $c.Start()`,
	).
		Where(!m.File().Name.Matches(`_test\.go$`)).
		Report(`exec.CommandContext run with no cmd.WaitDelay: a grandchild holding the pipe blocks Wait after cancel; set WaitDelay before running`)
}
