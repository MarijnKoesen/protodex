// SPDX-License-Identifier: Apache-2.0

package trust_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/trust"
	"github.com/marijnkoesen/protodex/internal/trust/trusttest"
)

const aud = "https://pd.example.com"

type exchangeEnv struct {
	fixture
	gh, k8s *trusttest.Issuer
	url     string
	logs    *syncBuffer
	ex      *trust.Exchanger
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newExchangeEnv(t *testing.T) exchangeEnv {
	t.Helper()
	e := exchangeEnv{fixture: newFixture(t), gh: trusttest.NewIssuer(t), k8s: trusttest.NewIssuer(t), logs: &syncBuffer{}}
	cfg := config.Default().CITrust
	cfg.Enabled = true
	cfg.GitHubIssuer = e.gh.URL()
	cfg.Kubernetes = []config.KubernetesIssuer{{Issuer: e.k8s.URL(), JWKSURL: e.k8s.JWKSURL()}}
	e.ex = trust.New(cfg, e.st, slog.New(slog.NewTextHandler(e.logs, nil)))
	mux := http.NewServeMux()
	e.ex.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e.url = srv.URL + trust.ExchangePath
	ctx := context.Background()
	if _, err := trust.Add(ctx, e.st, trust.Spec{Name: "gha", Module: "acme/api", User: "ci-bot", Issuer: e.gh.URL(), Audience: aud,
		Repository: "acme/api", Subject: "repo:acme/api:*", Ref: "refs/heads/main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := trust.Add(ctx, e.st, trust.Spec{Name: "k8s", Module: "acme/api", User: "ci-bot", Provider: store.TrustKubernetes,
		Issuer: e.k8s.URL(), Audience: aud, Subject: "system:serviceaccount:ci:pusher", Role: store.ContributorRead, TTL: 5 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	return e
}

func ghClaims() map[string]any {
	return map[string]any{"sub": "repo:acme/api:ref:refs/heads/main", "repository": "acme/api", "ref": "refs/heads/main"}
}

func (e exchangeEnv) post(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(e.url, "application/json", strings.NewReader(body)) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("response %d %q: %v", resp.StatusCode, b, err)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	return resp.StatusCode, out
}

func (e exchangeEnv) exchange(t *testing.T, token, module string) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(trust.Request{Token: token, Module: module})
	return e.post(t, string(b))
}

func TestExchangeGitHub(t *testing.T) {
	e := newExchangeEnv(t)
	ctx := context.Background()
	start := time.Now()
	code, out := e.exchange(t, e.gh.Token(aud, 5*time.Minute, ghClaims()), "acme/api")
	if code != http.StatusOK {
		t.Fatalf("exchange: %d %v", code, out)
	}
	secret, _ := out["token"].(string)
	if !auth.ValidSecretFormat(secret) || out["role"] != "write" || out["module"] != "acme/api" {
		t.Fatalf("response = %v", out)
	}
	exp, err := time.Parse(time.RFC3339, out["expires_at"].(string))
	if err != nil || exp.Before(start.Add(14*time.Minute)) || exp.After(time.Now().Add(15*time.Minute)) {
		t.Fatalf("expires_at = %v (%v), want ~15m", exp, err)
	}
	tok, err := e.st.GetTokenByHash(ctx, auth.HashSecret(secret))
	if err != nil {
		t.Fatal(err)
	}
	if tok.OwnerID != e.bot.ID || tok.ModuleID != e.module.ID || tok.ModuleRole != store.ContributorWrite || tok.Name != "ci:gha" || tok.ExpiresAt == nil {
		t.Fatalf("stored token = %+v", tok)
	}
	// The minted token authenticates as the bot and stops working on expiry.
	a := &auth.Authenticator{Store: e.st}
	if id, err := a.Authenticate(ctx, secret, "test"); err != nil || id.Owner.ID != e.bot.ID || id.Token.ModuleID != e.module.ID {
		t.Fatalf("Authenticate = %+v, %v", id, err)
	}
	a.Now = func() time.Time { return tok.ExpiresAt.Add(time.Second) }
	if _, err := a.Authenticate(ctx, secret, "test"); err == nil {
		t.Fatal("expired minted token still authenticates")
	}
	if !strings.Contains(e.logs.String(), "ci token issued") {
		t.Errorf("issue not logged: %s", e.logs)
	}
	// A later exchange sweeps expired scoped tokens.
	e.ex.Now = func() time.Time { return tok.ExpiresAt.Add(time.Second) }
	if code, out := e.exchange(t, e.gh.Token(aud, time.Hour, ghClaims()), "acme/api"); code != http.StatusOK {
		t.Fatalf("second exchange: %d %v", code, out)
	}
	if _, err := e.st.GetTokenByHash(ctx, tok.Hash); err == nil {
		t.Error("expired CI token not swept")
	}
}

func TestExchangeKubernetes(t *testing.T) {
	e := newExchangeEnv(t)
	code, out := e.exchange(t, e.k8s.Token(aud, 5*time.Minute, map[string]any{"sub": "system:serviceaccount:ci:pusher"}), "acme/api")
	if code != http.StatusOK || out["role"] != "read" {
		t.Fatalf("exchange: %d %v", code, out)
	}
	exp, _ := time.Parse(time.RFC3339, out["expires_at"].(string))
	if exp.After(time.Now().Add(5 * time.Minute)) {
		t.Errorf("credential TTL not applied: %v", exp)
	}
	code, _ = e.exchange(t, e.k8s.Token(aud, 5*time.Minute, map[string]any{"sub": "system:serviceaccount:ci:other"}), "acme/api")
	if code != http.StatusUnauthorized {
		t.Errorf("other service account: %d", code)
	}
}

func TestExchangeDenied(t *testing.T) {
	e := newExchangeEnv(t)
	with := func(k string, v any) map[string]any {
		c := ghClaims()
		c[k] = v
		return c
	}
	untrusted := trusttest.NewIssuer(t)
	for name, tc := range map[string]struct {
		token, module, reason string
	}{
		"wrong audience":     {e.gh.Token("https://other.example.com", time.Minute, ghClaims()), "acme/api", "audience"},
		"wrong repository":   {e.gh.Token(aud, time.Minute, with("repository", "evil/api")), "acme/api", "repository rule"},
		"wrong ref":          {e.gh.Token(aud, time.Minute, with("ref", "refs/heads/dev")), "acme/api", "ref rule"},
		"wrong subject":      {e.gh.Token(aud, time.Minute, with("sub", "repo:evil/api:ref:refs/heads/main")), "acme/api", "subject rule"},
		"expired":            {e.gh.Token(aud, -time.Minute, ghClaims()), "acme/api", "expired"},
		"bad signature":      {e.gh.ForeignToken(aud, time.Minute, ghClaims()), "acme/api", "signature"},
		"unknown module":     {e.gh.Token(aud, time.Minute, ghClaims()), "acme/web", "unknown module"},
		"unknown owner":      {e.gh.Token(aud, time.Minute, ghClaims()), "nobody/api", "unknown owner"},
		"untrusted issuer":   {untrusted.Token(aud, time.Minute, ghClaims()), "acme/api", "untrusted issuer"},
		"issuer mismatch":    {e.gh.Token(aud, time.Minute, with("iss", e.k8s.URL())), "acme/api", "k8s:"},
		"github on k8s cred": {e.k8s.Token(aud, time.Minute, ghClaims()), "acme/api", "subject rule"},
		"not a jwt":          {"pdx_abc", "acme/api", "malformed"},
	} {
		code, out := e.exchange(t, tc.token, tc.module)
		if code != http.StatusUnauthorized || out["error"] != "token exchange denied" || out["token"] != nil {
			t.Errorf("%s: %d %v", name, code, out)
		}
		if !strings.Contains(e.logs.String(), tc.reason) {
			t.Errorf("%s: reason %q not logged:\n%s", name, tc.reason, e.logs)
		}
	}
	// A credential whose module was deleted can no longer mint.
	if err := e.st.DeleteModule(context.Background(), e.module.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.exchange(t, e.gh.Token(aud, time.Minute, ghClaims()), "acme/api"); code != http.StatusUnauthorized {
		t.Errorf("deleted module: %d", code)
	}
}

func TestExchangeNoCredentialForIssuer(t *testing.T) {
	e := newExchangeEnv(t)
	if err := trust.Remove(context.Background(), e.st, "gha"); err != nil {
		t.Fatal(err)
	}
	code, _ := e.exchange(t, e.gh.Token(aud, time.Minute, ghClaims()), "acme/api")
	if code != http.StatusUnauthorized || !strings.Contains(e.logs.String(), "no trust credential") {
		t.Fatalf("removed credential: %d\n%s", code, e.logs)
	}
}

func TestExchangeBadRequest(t *testing.T) {
	e := newExchangeEnv(t)
	for _, body := range []string{"", "{", `{"token":"x"}`, `{"module":"acme/api"}`, `{"token":"x","module":"acme/api","role":"admin"}`} {
		if code, _ := e.post(t, body); code != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", body, code)
		}
	}
	resp, err := http.Get(e.url) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", resp.StatusCode)
	}
}

type denyAll struct{ calls int }

func (d *denyAll) Allow(context.Context, string) bool { d.calls++; return false }

func TestExchangeRateLimited(t *testing.T) {
	e := newExchangeEnv(t)
	l := &denyAll{}
	e.ex.Limiter = l
	if code, _ := e.exchange(t, e.gh.Token(aud, time.Minute, ghClaims()), "acme/api"); code != http.StatusTooManyRequests || l.calls != 1 {
		t.Fatalf("rate limited: %d, %d calls", code, l.calls)
	}
}

func TestExchangeTTLCapped(t *testing.T) {
	e := newExchangeEnv(t)
	cfg := config.Default().CITrust
	cfg.GitHubIssuer = e.gh.URL()
	cfg.TokenTTL = config.Duration(3 * time.Hour) // invalid config must still not exceed the cap
	ex := trust.New(cfg, e.st, slog.New(slog.DiscardHandler))
	resp, err := ex.Exchange(context.Background(), trust.Request{Token: e.gh.Token(aud, time.Minute, ghClaims()), Module: "acme/api"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExpiresAt.After(time.Now().Add(config.MaxCITokenTTL)) {
		t.Fatalf("expires_at %v exceeds the cap", resp.ExpiresAt)
	}
}

func TestExchangeDefaultRateLimit(t *testing.T) {
	e := newExchangeEnv(t) // trust.New's own limiter, nothing injected
	for i := range trust.ExchangeBurst {
		if code, _ := e.post(t, "{"); code != http.StatusBadRequest {
			t.Fatalf("request %d within burst: %d", i, code)
		}
	}
	if code, out := e.post(t, "{"); code != http.StatusTooManyRequests {
		t.Fatalf("request past burst: %d %v", code, out)
	}
}

// stalledIssuer accepts connections but never answers; it counts requests.
type stalledIssuer struct {
	url  string
	hits chan struct{}
}

func newStalledIssuer(t *testing.T) *stalledIssuer {
	t.Helper()
	release := make(chan struct{})
	s := &stalledIssuer{hits: make(chan struct{}, 100)}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		s.hits <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: unblock handlers
	s.url = srv.URL
	return s
}

func TestExchangeStalledIssuer(t *testing.T) {
	e := newExchangeEnv(t)
	stalled := newStalledIssuer(t)
	cfg := config.Default().CITrust
	cfg.GitHubIssuer = stalled.url // discovery of the GitHub issuer hangs
	cfg.Kubernetes = []config.KubernetesIssuer{{Issuer: e.k8s.URL(), JWKSURL: e.k8s.JWKSURL()}}
	ex := trust.New(cfg, e.st, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	k8sReq := trust.Request{Token: e.k8s.Token(aud, time.Minute, map[string]any{"sub": "system:serviceaccount:ci:pusher"}), Module: "acme/api"}
	if _, err := ex.Exchange(ctx, k8sReq); err != nil { // caches the k8s keys
		t.Fatal(err)
	}
	ghClaimsStalled := ghClaims()
	ghClaimsStalled["iss"] = stalled.url
	ghReq := trust.Request{Token: e.gh.Token(aud, time.Minute, ghClaimsStalled), Module: "acme/api"}

	// No credential for the stalled issuer yet: denied without contacting it.
	if _, err := ex.Exchange(ctx, ghReq); !errors.Is(err, trust.ErrDenied) {
		t.Fatalf("no credential: %v", err)
	}
	if len(stalled.hits) != 0 {
		t.Fatal("issuer contacted without a matching credential")
	}
	if _, err := trust.Add(ctx, e.st, trust.Spec{Name: "stalled", Module: "acme/api", User: "ci-bot", Issuer: stalled.url,
		Audience: aud, Repository: "acme/api"}); err != nil {
		t.Fatal(err)
	}

	// Two requests for the stalled issuer: one discovering, one waiting for it.
	done := make(chan error, 2)
	for range 2 {
		go func() {
			sctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			defer cancel()
			_, err := ex.Exchange(sctx, ghReq)
			done <- err
		}()
	}
	select {
	case <-stalled.hits:
	case <-time.After(5 * time.Second):
		t.Fatal("stalled issuer never contacted")
	}
	// Meanwhile the cached healthy issuer keeps working.
	start := time.Now()
	if _, err := ex.Exchange(ctx, k8sReq); err != nil {
		t.Fatalf("healthy issuer during stall: %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("healthy issuer blocked for %v", d)
	}
	// The stalled requests end with their context, not with the issuer.
	for range 2 {
		select {
		case err := <-done:
			if err == nil || errors.Is(err, trust.ErrDenied) {
				t.Errorf("stalled issuer: err = %v, want a discovery error", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("exchange for a stalled issuer did not give up")
		}
	}
}
