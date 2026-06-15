//go:build unix

package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpFlock_SerializesConcurrentHolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock-op.flock")

	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup

	for i := range 3 {
		wg.Go(func() {
			h, err := acquireOpFlock(context.Background(), path, nil)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer h.release()
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
		})
	}
	wg.Wait()

	if len(order) != 3 {
		t.Errorf("expected 3 holders, got %d", len(order))
	}
}

func TestOpFlock_EmitsWaitNoticeAfter250ms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock-op.flock")

	h1, err := acquireOpFlock(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		h1.release()
	}()

	var buf bytes.Buffer
	h2, err := acquireOpFlock(context.Background(), path, &buf)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer h2.release()

	if !strings.Contains(buf.String(), "waiting flock=lock-op") {
		t.Errorf("missing wait notice: %q", buf.String())
	}
}

func TestOpFlock_TimeoutAborts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock-op.flock")
	t.Setenv("LOTO_FLOCK_TIMEOUT", "100ms")

	h1, err := acquireOpFlock(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h1.release()

	start := time.Now()
	_, err = acquireOpFlock(context.Background(), path, nil)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrFlockTimeout) {
		t.Fatalf("want ErrFlockTimeout, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}

func TestNextBackoff_DoublesUpToCap(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{flockPollInitial, 50 * time.Millisecond},
		{50 * time.Millisecond, 100 * time.Millisecond},
		{200 * time.Millisecond, flockPollMax},
		{flockPollMax, flockPollMax},
		{flockPollMax + time.Second, flockPollMax},
	}
	for _, c := range cases {
		if got := nextBackoff(c.in); got != c.want {
			t.Errorf("nextBackoff(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestJitter_StaysWithinBand(t *testing.T) {
	const base = 100 * time.Millisecond
	const lo = time.Duration(float64(base) * (1 - flockJitterFactor))
	const hi = time.Duration(float64(base) * (1 + flockJitterFactor))
	differs := false
	for range 64 {
		got := jitter(base)
		if got < lo || got > hi {
			t.Errorf("jitter(%v) = %v, want in [%v, %v]", base, got, lo, hi)
		}
		if got != base {
			differs = true
		}
	}
	if !differs {
		t.Error("jitter never deviated from base; randomness may be broken")
	}
}

func TestOpFlock_CtxCancelAborts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock-op.flock")
	t.Setenv("LOTO_FLOCK_TIMEOUT", "10s")

	h1, err := acquireOpFlock(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h1.release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = acquireOpFlock(ctx, path, nil)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("ctx cancel didn't preempt promptly: %v", elapsed)
	}
}

// TestFlockLimitFromEnv_MalformedWarnsAndDefaults is the loto-d4is regression:
// a set-but-unparseable LOTO_FLOCK_TIMEOUT previously fell back to the 30s
// default in silence, so the long wait read as a hang. It must now fall back
// AND warn to the provided writer.
func TestFlockLimitFromEnv_MalformedWarnsAndDefaults(t *testing.T) {
	cases := []struct{ name, val string }{
		{"garbage", "soon"},
		{"no-unit", "30"},
		{"non-positive", "-5s"},
		{"zero", "0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOTO_FLOCK_TIMEOUT", tc.val)
			var warn bytes.Buffer
			got := flockLimitFromEnv(&warn)
			if got != flockDefaultLimit {
				t.Errorf("malformed %q: limit = %v, want default %v", tc.val, got, flockDefaultLimit)
			}
			if !strings.Contains(warn.String(), "LOTO_FLOCK_TIMEOUT") || !strings.Contains(warn.String(), "∇") {
				t.Errorf("malformed %q: want a ∇ warning naming the var, got %q", tc.val, warn.String())
			}
		})
	}
}

// TestFlockLimitFromEnv_ValidNoWarn: a valid value parses with no warning, and
// a nil warnW must not panic.
func TestFlockLimitFromEnv_ValidNoWarn(t *testing.T) {
	t.Setenv("LOTO_FLOCK_TIMEOUT", "2s")
	var warn bytes.Buffer
	if got := flockLimitFromEnv(&warn); got != 2*time.Second {
		t.Errorf("limit = %v, want 2s", got)
	}
	if warn.Len() != 0 {
		t.Errorf("valid value must not warn, got %q", warn.String())
	}
	if got := flockLimitFromEnv(nil); got != 2*time.Second {
		t.Errorf("nil warnW: limit = %v, want 2s", got)
	}
}
