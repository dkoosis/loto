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
