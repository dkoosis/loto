package domain

import (
	"errors"
	"testing"
)

func TestParseHoldRef_RoundTrips(t *testing.T) {
	cases := []HoldRef{
		{Owner: tcAlice, Epoch: 1},
		{Owner: tcAlice, Epoch: 0},   // legacy rows read as epoch 0, not "absent"
		{Owner: "a@b", Epoch: 12},    // owner containing '@' survives the last-@ split
		{Owner: tcAlice, Epoch: 999}, // multi-digit
	}
	for _, want := range cases {
		got, err := ParseHoldRef(want.String())
		if err != nil {
			t.Errorf("%s: %v", want, err)
			continue
		}
		if got != want {
			t.Errorf("round trip: got %v, want %v", got, want)
		}
	}
}

func TestParseHoldRef_Malformed(t *testing.T) {
	// Every one of these would, if accepted, silently weaken a compare-and-swap
	// into "break whatever is there".
	for _, in := range []string{"", tcAlice, "@1", "alice@", "alice@-1", "alice@x", "alice@1@", "alice@ 1"} {
		if got, err := ParseHoldRef(in); !errors.Is(err, ErrMalformedHoldRef) {
			t.Errorf("%q: want ErrMalformedHoldRef, got %v (%v)", in, err, got)
		}
	}
}

func TestHoldRefsEqual_IsSetShapedAndExact(t *testing.T) {
	a := []HoldRef{{Owner: tcBob, Epoch: 2}, {Owner: tcAlice, Epoch: 1}}
	b := []HoldRef{{Owner: tcAlice, Epoch: 1}, {Owner: tcBob, Epoch: 2}}
	SortHoldRefs(a)
	SortHoldRefs(b)
	if !HoldRefsEqual(a, b) {
		t.Errorf("input order must not matter after SortHoldRefs")
	}
	// A subset is NOT equal: a holder the caller never named is one it never
	// authorized breaking.
	if HoldRefsEqual(a[:1], b) {
		t.Errorf("subset must not compare equal")
	}
	// Same owner, different generation.
	c := []HoldRef{{Owner: tcAlice, Epoch: 1}, {Owner: tcBob, Epoch: 3}}
	if HoldRefsEqual(a, c) {
		t.Errorf("a bumped epoch must break equality")
	}
}

func TestFormatHoldRefs_EmptyIsNamed(t *testing.T) {
	if got := FormatHoldRefs(nil); got != "none" {
		t.Errorf("empty set must render as %q, got %q — a blank field reads as missing", "none", got)
	}
	got := FormatHoldRefs([]HoldRef{{Owner: tcAlice, Epoch: 1}, {Owner: tcBob, Epoch: 2}})
	if got != "alice@1,bob@2" {
		t.Errorf("got %q", got)
	}
}
