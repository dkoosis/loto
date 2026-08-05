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
//     pid-liveness.
//   - forbidigo (see golangci.snippet.yml): context.Background in daemons.
//   - a standalone analyzer (../omitemptyscalar): omitempty on meaningful
//     zero-value fields — ruleguard can't read struct tags (README §rule 3).
//
// A consuming repo copies this file to .golangci-rules/bugclasses.go verbatim.
// That copy drifts silently when the pack changes, so the pack is versioned:
// bump the token below on ANY rule edit; each repo runs check-pack-drift.sh in
// CI to compare its copy's hash against upstream (golangci.snippet.yml §drift).
//
//pack:version v1
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
// rule file and drift-check the copy — see check-pack-drift.sh. Do not wire an
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
		// Skip tests (fixtures aren't production input) and in-memory readers
		// (strings/bytes.NewReader is bounded by an already-loaded value —
		// the same carve-out the alloc-bounds lens makes for ReadAll).
		Where(!m.File().Name.Matches(`_test\.go$`) &&
			!m["r"].Text.Matches(`^(strings|bytes)\.NewReader`)).
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
	// strings.Contains / HasPrefix / HasSuffix over an .Error() string.
	// Test files legitimately assert on error text; the defect is production
	// control flow keyed on a message. Exclude _test.go from both groups.
	m.Match(
		`strings.Contains($err.Error(), $_)`,
		`strings.HasPrefix($err.Error(), $_)`,
		`strings.HasSuffix($err.Error(), $_)`,
	).
		Where(m["err"].Type.Implements(`error`) &&
			!m.File().Name.Matches(`_test\.go$`)).
		Report(`error classified by substring; use typed sentinels + errors.Is/As`)

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
