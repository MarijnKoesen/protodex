// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/marijnkoesen/protodex/internal/auth"
)

func TestRateLimiter(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_000_000, 0)
	l := auth.NewRateLimiter(1, 3) // 3 at once, then one per second
	l.Now = func() time.Time { return now }
	for i := range 3 {
		// Different ports of one host share the bucket.
		if !l.Allow(ctx, "192.0.2.1:"+strconv.Itoa(1000+i)) {
			t.Fatalf("request %d within burst refused", i)
		}
	}
	if l.Allow(ctx, "192.0.2.1:5000") {
		t.Fatal("request past burst allowed")
	}
	if !l.Allow(ctx, "192.0.2.2:5000") {
		t.Fatal("other client limited by the first")
	}
	now = now.Add(500 * time.Millisecond)
	if l.Allow(ctx, "192.0.2.1:5000") {
		t.Fatal("allowed before a token refilled")
	}
	now = now.Add(500 * time.Millisecond)
	if !l.Allow(ctx, "192.0.2.1:5000") {
		t.Fatal("refused after refill")
	}
	if l.Allow(ctx, "192.0.2.1:5000") {
		t.Fatal("refill exceeded the rate")
	}
	now = now.Add(time.Hour) // refill is capped at burst
	for i := range 3 {
		if !l.Allow(ctx, "192.0.2.1") {
			t.Fatalf("request %d after idle refused", i)
		}
	}
	if l.Allow(ctx, "192.0.2.1") {
		t.Fatal("idle time refilled past burst")
	}
}

func TestRateLimiterConcurrent(t *testing.T) {
	l := auth.NewRateLimiter(0, 10) // no refill: exactly burst requests pass
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 50 {
		wg.Go(func() {
			if l.Allow(context.Background(), "198.51.100.7:1234") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed != 10 {
		t.Fatalf("allowed %d, want 10", allowed)
	}
}
