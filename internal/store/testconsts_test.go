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
	// tcPkgStore is the representative claim prefix — a real package path, so
	// the overlap cases read as territory rather than as opaque fixtures.
	tcPkgStore = "internal/store"
	// tcOtherHost is a host that never equals tcHost — the cross-host fixture
	// for liveness probes that gate on host equality (loto-u2p7).
	tcOtherHost = "other-host"
)
