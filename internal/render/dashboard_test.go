package render

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestEmitLLMDashboardEventHeld(t *testing.T) {
	var buf bytes.Buffer
	ev := DashboardEvent{
		Time:   time.Date(2026, 4, 30, 14, 32, 0, 0, time.UTC),
		Kind:   "held",
		Agent:  "GreenCastle",
		Target: "internal/store/store.go",
		Intent: "edit store",
	}
	if err := EmitLLMDashboardEvent(&buf, ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"held", "GreenCastle", "store.go", "intent:edit store", "ts:2026-04-30T14:32:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
}

func TestEmitLLMDashboardEventMsg(t *testing.T) {
	var buf bytes.Buffer
	ev := DashboardEvent{
		Time:  time.Date(2026, 4, 30, 14, 34, 0, 0, time.UTC),
		Kind:  "msg",
		Agent: "BlueOak",
		To:    "GreenCastle",
		Body:  "need 5min on store.go",
	}
	if err := EmitLLMDashboardEvent(&buf, ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"msg", "from:BlueOak", "to:GreenCastle", "need 5min"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
}

func TestEmitHumanDashboardEventReserved(t *testing.T) {
	var buf bytes.Buffer
	ev := DashboardEvent{
		Time:   time.Date(2026, 4, 30, 14, 32, 0, 0, time.UTC),
		Kind:   "reserved",
		Agent:  "GreenCastle",
		Target: "internal/store/**",
		Intent: "scan",
	}
	if err := EmitHumanDashboardEvent(&buf, ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"reserved", "GreenCastle", "internal/store/**", "scan"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
}
