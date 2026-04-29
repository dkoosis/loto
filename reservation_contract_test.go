package loto_test

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"loto"
)

type reservationOutput struct {
	count    int
	patterns []string
}

func TestReservationAPI_ExpectedBehaviour_When_ManagingReservationLifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   func(*testing.T) (*loto.LOTO, string)
		want    reservationOutput
		wantErr error
		inspect func(*testing.T, *loto.LOTO, string, reservationOutput)
	}{
		{
			name: "error_invalid_glob",
			input: func(t *testing.T) (*loto.LOTO, string) {
				l, _ := loto.New(t.TempDir())
				return l, ""
			},
			wantErr: loto.ErrInvalidGlob,
			inspect: func(t *testing.T, l *loto.LOTO, _ string, _ reservationOutput) {
				_, err := l.Reserve("a", "intent", "[\x00bad", 0)
				if !errors.Is(err, loto.ErrInvalidGlob) { t.Fatalf("want ErrInvalidGlob, got %v", err) }
			},
		},
		{
			name: "error_list_readdir",
			input: func(t *testing.T) (*loto.LOTO, string) {
				base := t.TempDir()
				_ = os.WriteFile(filepath.Join(base, "reservations"), []byte("x"), 0o600)
				l, _ := loto.New(base)
				return l, ""
			},
			wantErr: errors.New("list-reservations: readdir"),
			inspect: func(t *testing.T, l *loto.LOTO, _ string, _ reservationOutput) {
				_, err := l.ListReservations()
				if err == nil || !strings.Contains(err.Error(), "list-reservations: readdir") { t.Fatalf("unexpected err: %v", err) }
			},
		},
		{
			name: "boundary_ttl_zero",
			input: func(t *testing.T) (*loto.LOTO, string) {
				l, _ := loto.New(t.TempDir())
				r, err := l.Reserve("a", "persist", "src/**", 0)
				if err != nil { t.Fatal(err) }
				if r.ExpiresAt != nil { t.Fatalf("ttl=0 must keep nil ExpiresAt") }
				return l, ""
			},
			want: reservationOutput{count: 1, patterns: []string{"src/**"}},
		},
		{
			name: "happy_prunes_expired",
			input: func(t *testing.T) (*loto.LOTO, string) {
				base := t.TempDir()
				l, _ := loto.New(base)
				_, _ = l.Reserve("a", "active", "pkg/**", 0)
				future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
				past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
				resDir := filepath.Join(base, "reservations")
				_ = os.WriteFile(filepath.Join(resDir, "future.tag"), []byte(`{"agent_id":"b","intent":"future","pattern":"web/**","created_at":"2026-01-01T00:00:00Z","expires_at":"`+future+`"}`), 0o600)
				_ = os.WriteFile(filepath.Join(resDir, "expired.tag"), []byte(`{"agent_id":"c","intent":"past","pattern":"old/**","created_at":"2026-01-01T00:00:00Z","expires_at":"`+past+`"}`), 0o600)
				return l, ""
			},
			want: reservationOutput{count: 2, patterns: []string{"pkg/**", "web/**"}},
		},
		{
			name: "happy_conflicting_reservations",
			input: func(t *testing.T) (*loto.LOTO, string) {
				base := t.TempDir()
				l, _ := loto.New(base)
				target := filepath.Join(base, "internal", "store", "db.go")
				_ = os.MkdirAll(filepath.Dir(target), 0o755)
				_ = os.WriteFile(target, []byte("package store"), 0o600)
				_, _ = l.Reserve("b", "refactor", filepath.Join(base, "internal", "**"), time.Hour)
				return l, target
			},
			want: reservationOutput{count: 1},
			inspect: func(t *testing.T, l *loto.LOTO, target string, _ reservationOutput) {
				c, err := l.ConflictingReservations(target)
				if err != nil { t.Fatal(err) }
				if len(c) != 1 { t.Fatalf("want 1 conflict, got %d", len(c)) }
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, target := tc.input(t)
			if tc.wantErr != nil {
				tc.inspect(t, l, target, reservationOutput{})
				return
			}
			gotList, err := l.ListReservations()
			if err != nil { t.Fatal(err) }
			got := reservationOutput{count: len(gotList)}
			for _, r := range gotList { got.patterns = append(got.patterns, r.Pattern) }
			sort.Strings(got.patterns)
			sort.Strings(tc.want.patterns)
			if tc.want.count != got.count { t.Fatalf("diff (-want +got):\ncount %d %d", tc.want.count, got.count) }
			if len(tc.want.patterns) > 0 && strings.Join(tc.want.patterns, ",") != strings.Join(got.patterns, ",") {
				t.Fatalf("diff (-want +got):\npatterns %v %v", tc.want.patterns, got.patterns)
			}
			if tc.inspect != nil { tc.inspect(t, l, target, got) }
		})
	}
}
