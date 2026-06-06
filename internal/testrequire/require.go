package testrequire

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func ErrorIs(t *testing.T, err, target error, msgAndArgs ...any) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("expected error %v to match %v", err, target)
	}
}

func NoError(t *testing.T, err error, msgAndArgs ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func NotEmpty(t *testing.T, v any, msgAndArgs ...any) {
	t.Helper()
	if reflect.ValueOf(v).Len() == 0 {
		t.Fatalf("expected non-empty value")
	}
}

func False(t *testing.T, value bool, msgAndArgs ...any) {
	t.Helper()
	if value {
		t.Fatalf("expected false")
	}
}

func True(t *testing.T, value bool, msgAndArgs ...any) {
	t.Helper()
	if !value {
		t.Fatalf("expected true")
	}
}

func NotEqual(t *testing.T, want, got any, msgAndArgs ...any) {
	t.Helper()
	if reflect.DeepEqual(want, got) {
		t.Fatalf("expected values to differ: %#v", got)
	}
}

func Equal(t *testing.T, want, got any, msgAndArgs ...any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("expected %#v, got %#v", want, got)
	}
}

func NotContains(t *testing.T, s, substr string, msgAndArgs ...any) {
	t.Helper()
	if strings.Contains(s, substr) {
		t.Fatalf("expected %q not to contain %q", s, substr)
	}
}
