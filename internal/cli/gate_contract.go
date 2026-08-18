package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// GateContractVersion is the version of the hook↔binary interface this build
// speaks. Bump it whenever the PreToolUse hook starts depending on something a
// previous loto could not do — a new verb, a new flag, a changed exit code.
//
// The hook stamps the version IT needs into LOTO_GATE_CONTRACT. A binary that
// predates that number cannot honor the contract, and — because the gate fails
// open — it will wave writes through while the fleet believes it is protected.
//
// ‡ This is not speculative hardening. On 2026-08-12 the loto binary on PATH
// was 8 days stale and silently lacked the `guard` verb #228 had shipped. The
// door had rotted and nothing said so. Silent fail-open is strictly worse than
// no gate at all: no gate is a known gap, a rotted gate is a false belief.
//
// Version log:
//
//	1 — `check --gate`, `guard`, `beacon`, `check --cwd-unknown`.
const GateContractVersion = 1

// gateContractEnv is where the hook stamps the version it needs. An env var
// rather than a per-verb flag: `check --gate`, `guard` and `beacon` are all
// gate surfaces, and a flag would have to be threaded through each one's
// parser and each one's usage text to say the same thing.
const gateContractEnv = "LOTO_GATE_CONTRACT"

// warnIfContractStale emits one ⚠ row per session when the hook needs a newer
// contract than this binary speaks, and returns whether it warned.
//
// Never blocks, never changes an exit code: a stale binary is a degraded gate,
// not a reason to refuse the caller's work. The row goes to stderr for the same
// reason every fail-open notice does (loto-tzmv.8) — a PreToolUse hook exiting
// 0 does not surface stdout to the model, so a stdout warning announces the
// gate's own rot into a channel nobody reads.
//
// Silent on the healthy path. A version equal to or below this build's is the
// normal case and must cost nothing, or the fleet learns to filter ⚠ rows.
func warnIfContractStale(stderr io.Writer) bool {
	want, ok := wantedContract()
	if !ok || want <= GateContractVersion {
		return false
	}
	if !claimContractWarning(want) {
		return false
	}
	fmt.Fprintf(stderr, "⚠ gate=contract-stale binary=%d hook=%d\n", GateContractVersion, want)
	fmt.Fprintf(stderr, "⚠ the loto on PATH predates this hook: it may wave writes through that a current build would refuse\n")
	fmt.Fprintf(stderr, "```bash\ncd ~/Projects/loto && git pull && make install\n```\n")
	return true
}

// wantedContract parses LOTO_GATE_CONTRACT. Unset, empty, or unparseable all
// answer "no opinion" rather than an error: the variable is set by a hook, and
// a hook that garbles it must not make loto noisier than one that omits it.
func wantedContract() (int, bool) {
	raw := strings.TrimSpace(os.Getenv(gateContractEnv))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// claimContractWarning reports whether THIS process should be the one to print
// the warning, and marks it printed. Once per session, not once per call: the
// gate runs on every tool call, and a row repeated hundreds of times per
// session is noise the model learns to skip.
//
// The marker lives in the temp dir rather than ~/.loto: "once per session" only
// has to hold for the session's life, and a temp file expires on its own
// instead of accreting a file per session forever. An unresolvable session id
// (direct CLI use, no Claude Code env) warns every time — rare by construction,
// and the alternative is one shared marker that silences every session after
// the first.
func claimContractWarning(want int) bool {
	sid, pinned := sessionUUID()
	if !pinned {
		return true
	}
	dir := filepath.Join(os.TempDir(), "loto-gate-contract")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return true // can't dedupe → warn, since the warning matters more
	}
	// The wanted version is part of the name: a hook upgraded mid-session is a
	// new fact and deserves to be said again.
	marker := filepath.Join(dir, sanitizeMarker(sid)+"."+strconv.Itoa(want))
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false // already claimed by an earlier call in this session
	}
	_ = f.Close()
	return true
}

// sanitizeMarker maps a session id onto one path-safe filename component. The
// id arrives from the environment, so it is not trusted to be a UUID.
func sanitizeMarker(sid string) string {
	var b strings.Builder
	for _, r := range sid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
