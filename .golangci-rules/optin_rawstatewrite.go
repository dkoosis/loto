// Rule 2 of the cross-repo defect pack (decision nug d7ea466a8172), shipped as
// its own file because it is OPT-IN. A raw os.WriteFile is legitimate for
// scratch/temp output, so flagging every call site is noise until a repo has
// moved its DURABLE state to dkoosis/atomicfile (ccp-sbp.5). gocritic's
// ruleguard checker has no per-group disable, so inclusion in the `rules`
// glob is the on/off switch:
//
//	rules: "${base-path}/.golangci-rules/bugclasses.go"   # rule 2 OFF
//	rules: "${base-path}/.golangci-rules/*.go"            # rule 2 ON (post-adoption)
//
// See README §rule 2. Drift-checked like bugclasses.go when a repo adopts it.
//
// _test.go is excluded the same way rules 1 and 4 exclude it (rules.go): a
// test writing scratch files is not durable state (upstreamed
// ccp-vrzj — a repo that adopted this rule no longer needs a local diff for
// the same carve-out).
//
//pack:version v2
package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

//doc:summary raw file write where durable state wants an atomic write
//doc:before  os.WriteFile(statePath, data, 0o644)
//doc:after   atomicfile.WriteFile(statePath, data, 0o644)
//doc:tags    durability
func rawStateWrite(m dsl.Matcher) {
	m.Match(
		`os.WriteFile($_, $_, $_)`,
		`ioutil.WriteFile($_, $_, $_)`,
	).
		Where(!m.File().Name.Matches(`_test\.go$`)).
		Report(`raw file write; use dkoosis/atomicfile for durable state (torn-write on crash)`)
}
