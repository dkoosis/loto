package gate

import (
	"context"
	"fmt"
)

// IntegrationRef is the "machine-verified integrated state" ref every
// envelope's preimages are captured from and every admission re-validates
// against (git-gate.md's four authority levels). ONE well-known name so every
// caller in the fleet agrees on where it lives, without needing to pass it
// around out of band.
const IntegrationRef = "refs/loto/integration"

// ResolveIntegrationRef returns IntegrationRef's current commit SHA,
// bootstrapping it to HEAD on first use if it does not exist yet.
//
// ‡ Promotion (a later bead, loto-ovno.7/.8) is what ADVANCES this ref once a
// candidate is accepted and merged; nothing in this codebase does that yet.
// Until it does, every submit validates against the SAME frozen point this
// bootstrap picks — which is correct, not a workaround: "integration" is
// meant to be a stable reference point, and a repo with no promotion pipeline
// running yet legitimately has no integrated state beyond "wherever HEAD was
// when the first candidate was submitted."
//
// ‡ HEAD, not a hardcoded "main": this repo's own shared checkout stays on
// main by convention (shared-main-guard), so HEAD already resolves there in
// practice, and reading HEAD rather than a literal branch name keeps this
// correct for a checkout on any branch without hardcoding an assumption this
// package has no business making.
func ResolveIntegrationRef(ctx context.Context, repoTop string) (string, error) {
	g := gitRunner{repoTop: repoTop}
	if sha, err := g.run(ctx, "rev-parse", "--verify", "--quiet", IntegrationRef); err == nil {
		return sha, nil
	}
	head, err := g.run(ctx, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", fmt.Errorf("gate: resolve HEAD to bootstrap %s: %w", IntegrationRef, err)
	}
	if err := UpdateRefsTx(ctx, repoTop, []RefUpdate{{Verb: VerbCreate, Ref: IntegrationRef, NewSHA: head}}); err != nil {
		// A concurrent bootstrap from a peer process lost the race — re-read
		// rather than fail; either value is a valid bootstrap point and only
		// one caller needs to have won.
		if sha, rerr := g.run(ctx, "rev-parse", "--verify", "--quiet", IntegrationRef); rerr == nil {
			return sha, nil
		}
		return "", fmt.Errorf("gate: bootstrap %s: %w", IntegrationRef, err)
	}
	return head, nil
}
