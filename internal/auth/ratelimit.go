// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"net"
	"sync"
	"time"
)

// RateLimiter is an in-process Limiter: a token bucket per client key
// holding up to Burst requests, refilled at Rate per second. Keys of the
// form host:port are limited per host, so a client cannot dodge the limit
// by opening new connections. Idle buckets are dropped. It is safe for
// concurrent use; the zero value is not, use NewRateLimiter.
type RateLimiter struct {
	rate  float64 // tokens per second
	burst float64
	// Now defaults to time.Now.
	Now func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// maxBuckets bounds memory if many distinct clients appear between sweeps;
// past it the limiter drops all full (idle) buckets immediately.
const maxBuckets = 100_000

// NewRateLimiter returns a limiter allowing burst requests at once and
// rate requests per second on average per client. burst < 1 is treated
// as 1.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return &RateLimiter{rate: rate, burst: float64(max(burst, 1)), buckets: map[string]*bucket{}}
}

func (l *RateLimiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Allow implements Limiter.
func (l *RateLimiter) Allow(_ context.Context, key string) bool {
	if host, _, err := net.SplitHostPort(key); err == nil {
		key = host
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(l.burst, b.tokens+el*l.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have refilled completely: forgetting them
// changes nothing. It runs at most once a minute unless the map is full.
func (l *RateLimiter) sweep(now time.Time) {
	if len(l.buckets) < maxBuckets && now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}
