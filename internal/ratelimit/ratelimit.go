// SPDX-License-Identifier: Apache-2.0

// Package ratelimit provides an in-memory, per-key token-bucket rate limiter
// and an HTTP middleware that applies per-IP and per-token limits.
//
// State is per process, which matches the single-process SQLite deployment;
// a multi-replica deployment limits per replica.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMaxKeys bounds the number of tracked keys.
const DefaultMaxKeys = 100_000

// Limiter is a token-bucket limiter keyed by string. Each key may burst up to
// Burst requests and then proceeds at Rate requests per second. A nil
// *Limiter allows everything.
type Limiter struct {
	rate    float64
	burst   float64
	maxKeys int
	now     func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a Limiter allowing rate requests per second with the given
// burst. It returns nil (allow everything) when rate <= 0. A burst below 1 is
// raised to max(1, ceil(rate)).
func New(rate float64, burst int) *Limiter {
	return newLimiter(rate, burst, DefaultMaxKeys, time.Now)
}

func newLimiter(rate float64, burst, maxKeys int, now func() time.Time) *Limiter {
	if rate <= 0 {
		return nil
	}
	if burst < 1 {
		burst = int(math.Max(1, math.Ceil(rate)))
	}
	return &Limiter{rate: rate, burst: float64(burst), maxKeys: maxKeys, now: now, buckets: map[string]*bucket{}}
}

// Allow reports whether one request for key may proceed now. It implements
// auth.Limiter.
func (l *Limiter) Allow(_ context.Context, key string) bool {
	ok, _ := l.Take(key)
	return ok
}

// Take is like Allow but also returns, for a rejected request, how long to
// wait before retrying.
func (l *Limiter) Take(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		l.makeRoom(now)
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else if el := now.Sub(b.last); el > 0 {
		b.tokens = math.Min(l.burst, b.tokens+el.Seconds()*l.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

// makeRoom keeps the map below maxKeys: first dropping buckets that have
// fully refilled (indistinguishable from new ones), then arbitrary ones.
func (l *Limiter) makeRoom(now time.Time) {
	if len(l.buckets) < l.maxKeys {
		return
	}
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
	for k := range l.buckets {
		if len(l.buckets) < l.maxKeys {
			return
		}
		delete(l.buckets, k)
	}
}

// Config configures Middleware. A zero rate disables the matching limit.
type Config struct {
	IPRate     float64
	IPBurst    int
	TokenRate  float64
	TokenBurst int
	// TrustForwardedFor derives the client IP from the last X-Forwarded-For
	// entry (the one appended by the trusted reverse proxy) instead of the
	// TCP peer. Enable only behind a proxy that sets the header.
	TrustForwardedFor bool
	// Exempt reports paths that are never limited (probes, metrics).
	Exempt func(path string) bool
	// OnLimited is called for each rejected request (kind "ip" or "token").
	OnLimited func(kind string)
}

// Middleware rejects requests over the per-IP limit (every request) or the
// per-token limit (requests presenting a bearer token) with HTTP 429 and a
// Connect-compatible resource_exhausted body.
func Middleware(cfg Config, next http.Handler) http.Handler {
	ip := New(cfg.IPRate, cfg.IPBurst)
	tok := New(cfg.TokenRate, cfg.TokenBurst)
	if ip == nil && tok == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.Exempt != nil && cfg.Exempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if ok, wait := ip.Take(ClientIP(r, cfg.TrustForwardedFor)); !ok {
			reject(w, cfg, "ip", wait)
			return
		}
		if tok != nil {
			if key := TokenKey(r.Header.Get("Authorization")); key != "" {
				if ok, wait := tok.Take(key); !ok {
					reject(w, cfg, "token", wait)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func reject(w http.ResponseWriter, cfg Config, kind string, wait time.Duration) {
	if cfg.OnLimited != nil {
		cfg.OnLimited(kind)
	}
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"code":"resource_exhausted","message":"rate limit exceeded"}`))
}

// TokenKey returns a stable, non-reversible key for the bearer secret in an
// Authorization header, or "" when there is none.
func TokenKey(header string) string {
	scheme, secret, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:16])
}

// ClientIP returns the key identifying the caller's network origin. IPv6
// addresses are grouped by /64 so one host cannot dodge the limit by rotating
// addresses within its prefix.
func ClientIP(r *http.Request, trustForwardedFor bool) string {
	raw := r.RemoteAddr
	if h, _, err := net.SplitHostPort(raw); err == nil {
		raw = h
	}
	if trustForwardedFor {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			raw = strings.TrimSpace(parts[len(parts)-1])
		}
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return raw
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}
