package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"loto/internal/domain"
)

// wireEnvelope is Envelope's on-disk shape. A separate type from Envelope
// itself for one reason: Go's encoding/json can't round-trip a nil vs.
// zero-value *BlobRef distinction through a plain struct tag alone without
// care, and pinning the wire shape here means a later field added to Envelope
// for in-memory convenience does not silently change the git-object bytes
// every existing envelope was hashed under.
type wireEnvelope struct {
	CandidateID string           `json:"candidate_id"`
	ProposalSHA string           `json:"proposal_sha"`
	Base        string           `json:"base"`
	AgentUUID   string           `json:"agent_uuid"`
	SessionUUID string           `json:"session_uuid"`
	BeadID      string           `json:"bead_id"`
	WriteSet    []string         `json:"write_set"`
	Transitions []wireTransition `json:"transitions"`
	Ancestry    []wireAncestry   `json:"ancestry"`
	LeaseEpoch  map[string]int64 `json:"lease_epoch"`
}

type wireBlobRef struct {
	SHA  string `json:"sha"`
	Mode string `json:"mode"`
}

type wireTransition struct {
	Path     string       `json:"path"`
	Expected *wireBlobRef `json:"expected"`
	Result   *wireBlobRef `json:"result"`
}

type wireAncestry struct {
	Path string `json:"path"`
}

func toWireBlobRef(b *BlobRef) *wireBlobRef {
	if b == nil {
		return nil
	}
	return &wireBlobRef{SHA: b.SHA, Mode: b.Mode}
}

func fromWireBlobRef(b *wireBlobRef) *BlobRef {
	if b == nil {
		return nil
	}
	return &BlobRef{SHA: b.SHA, Mode: b.Mode}
}

// Encode serializes e to its canonical byte form: field order fixed by
// wireEnvelope's declaration, slices sorted by Capture before this is ever
// called, and JSON marshaled with no HTML-escaping so the bytes a candidate
// author sees (`git cat-file -p`) match what was hashed byte for byte. Same
// Envelope value → byte-identical output on every call, which is what makes
// the object-store round-trip in WriteBlob/ReadBlob meaningful: two captures
// of the same state hash to the same blob.
func (e Envelope) Encode() ([]byte, error) {
	w := wireEnvelope{
		CandidateID: e.CandidateID,
		ProposalSHA: e.ProposalSHA,
		Base:        e.Base,
		AgentUUID:   string(e.AgentUUID),
		SessionUUID: string(e.SessionUUID),
		BeadID:      e.BeadID,
		WriteSet:    e.WriteSet,
		LeaseEpoch:  e.LeaseEpoch,
	}
	w.Transitions = make([]wireTransition, len(e.Transitions))
	for i, t := range e.Transitions {
		w.Transitions[i] = wireTransition{Path: t.Path, Expected: toWireBlobRef(t.Expected), Result: toWireBlobRef(t.Result)}
	}
	w.Ancestry = make([]wireAncestry, len(e.Ancestry))
	for i, a := range e.Ancestry {
		w.Ancestry[i] = wireAncestry(a)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(w); err != nil {
		return nil, fmt.Errorf("gate: encode envelope: %w", err)
	}
	return buf.Bytes(), nil
}

// DecodeEnvelope is Encode's inverse.
func DecodeEnvelope(data []byte) (Envelope, error) {
	var w wireEnvelope
	if err := json.Unmarshal(data, &w); err != nil {
		return Envelope{}, fmt.Errorf("gate: decode envelope: %w", err)
	}
	e := Envelope{
		CandidateID: w.CandidateID,
		ProposalSHA: w.ProposalSHA,
		Base:        w.Base,
		AgentUUID:   domain.AgentUUID(w.AgentUUID),
		SessionUUID: domain.SessionUUID(w.SessionUUID),
		BeadID:      w.BeadID,
		WriteSet:    w.WriteSet,
		LeaseEpoch:  w.LeaseEpoch,
	}
	e.Transitions = make([]Transition, len(w.Transitions))
	for i, t := range w.Transitions {
		e.Transitions[i] = Transition{Path: t.Path, Expected: fromWireBlobRef(t.Expected), Result: fromWireBlobRef(t.Result)}
	}
	e.Ancestry = make([]AncestryEntry, len(w.Ancestry))
	for i, a := range w.Ancestry {
		e.Ancestry[i] = AncestryEntry(a)
	}
	return e, nil
}

// WriteBlob content-addresses e into the repo's git object database via
// `git hash-object -w` and returns the resulting blob SHA. This is orthogonal
// to refs/loto/* — writing a git object touches no ref, so it carries none of
// the atomicity concerns a later bead's update-ref transaction owns. A blob
// written here is unreachable (no ref points at it) until that bead's admission
// path names it from refs/loto/candidates/<id>; an unreferenced blob is simply
// eligible for `git gc` like any other, which is the correct fate for an
// envelope whose candidate was never admitted.
func (e Envelope) WriteBlob(ctx context.Context, repoTop string) (string, error) {
	data, err := e.Encode()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "hash-object", "-w", "--stdin")
	cmd.Dir = repoTop
	cmd.Stdin = bytes.NewReader(data)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gate: hash-object envelope: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// ReadBlob reads back an envelope written by WriteBlob, by its blob SHA.
func ReadBlob(ctx context.Context, repoTop, sha string) (Envelope, error) {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-p", sha)
	cmd.Dir = repoTop
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Envelope{}, fmt.Errorf("gate: cat-file %s: %w: %s", sha, err, strings.TrimSpace(stderr.String()))
	}
	return DecodeEnvelope(out.Bytes())
}
