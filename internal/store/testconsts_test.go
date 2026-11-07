package store

const (
	tcAlice = "alice"
	tcBob   = "bob"
	tcCarol = "carol"
	tcTest  = "test"
	tcChmod = "chmod"
	tcXGo   = "x.go"
	tcAGo   = "a.go"
	tcBGo   = "b.go"
	tcCand1 = "cand-1"
	tcPing  = "ping"
	// tcPkgA is a directory target in the hold-overlap tests.
	tcPkgA = "pkg/a"
	// tcOtherHost is a host that never equals tcHost — the cross-host fixture
	// for liveness probes that gate on host equality (loto-u2p7).
	tcOtherHost = "other-host"
)
