// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterBurstAndRefill(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLimiter(1, 2, 10, func() time.Time { return now })
	if !l.Allow(nil, "a") || !l.Allow(nil, "a") { //nolint:staticcheck // ctx unused
		t.Fatal("burst of 2 should pass")
	}
	ok, wait := l.Take("a")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("third request: ok=%v wait=%v", ok, wait)
	}
	if !l.Allow(nil, "b") { //nolint:staticcheck
		t.Fatal("other key is independent")
	}
	now = now.Add(time.Second)
	if !l.Allow(nil, "a") { //nolint:staticcheck
		t.Fatal("token should have refilled")
	}
}

func TestNilLimiterAllows(t *testing.T) {
	var l *Limiter
	if ok, _ := l.Take("x"); !ok {
		t.Fatal("nil limiter must allow")
	}
	if New(0, 0) != nil {
		t.Fatal("rate 0 disables")
	}
}

func TestLimiterBoundsKeys(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(1, 1, 5, func() time.Time { return now })
	for i := range 100 {
		l.Allow(nil, string(rune('a'+i))) //nolint:staticcheck
		if len(l.buckets) > 5 {
			t.Fatalf("%d keys tracked", len(l.buckets))
		}
	}
}

func TestMiddleware(t *testing.T) {
	h := Middleware(Config{IPRate: 0.001, IPBurst: 3, TokenRate: 0.001, TokenBurst: 1,
		Exempt: func(p string) bool { return p == "/healthz" }},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	do := func(ip, path, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = ip + ":1234"
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// Token limit: second request with the same token is rejected even from another IP.
	if c := do("10.0.0.1", "/x", "Bearer tok").Code; c != 200 {
		t.Fatalf("first: %d", c)
	}
	w := do("10.0.0.2", "/x", "Bearer tok")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("token limit: %d %v", w.Code, w.Header())
	}
	// IP limit: burst 3 (one used above for 10.0.0.1 was the first; 10.0.0.2 used one).
	do("10.0.0.3", "/a", "")
	do("10.0.0.3", "/a", "")
	do("10.0.0.3", "/a", "")
	if c := do("10.0.0.3", "/a", "").Code; c != http.StatusTooManyRequests {
		t.Fatalf("ip limit: %d", c)
	}
	// Exempt paths are never limited.
	for range 10 {
		if c := do("10.0.0.3", "/healthz", "").Code; c != 200 {
			t.Fatalf("exempt: %d", c)
		}
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "[2001:db8:1:2:3:4:5:6]:99"
	if got := ClientIP(r, false); got != "2001:db8:1:2::/64" {
		t.Fatalf("v6 grouping: %s", got)
	}
	r.RemoteAddr = "192.0.2.1:5"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	if got := ClientIP(r, false); got != "192.0.2.1" {
		t.Fatalf("untrusted xff: %s", got)
	}
	if got := ClientIP(r, true); got != "198.51.100.7" {
		t.Fatalf("trusted xff uses last entry: %s", got)
	}
}

func TestTokenKey(t *testing.T) {
	if TokenKey("") != "" || TokenKey("Basic abc") != "" || TokenKey("Bearer ") != "" {
		t.Fatal("non-bearer must have no key")
	}
	if TokenKey("Bearer abc") != TokenKey("bearer  abc") || TokenKey("Bearer abc") == TokenKey("Bearer abd") {
		t.Fatal("key must depend on the secret only")
	}
}
