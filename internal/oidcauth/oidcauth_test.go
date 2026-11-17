// SPDX-License-Identifier: Apache-2.0

package oidcauth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

type env struct {
	idp *fakeIDP
	st  *sqlite.Store
	h   *Handler
	srv *httptest.Server
	cl  *http.Client
}

func setup(t *testing.T, mod func(*config.OIDC)) *env {
	t.Helper()
	idp := newFakeIDP(t)
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := &env{idp: idp, st: st}
	mux := http.NewServeMux()
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	cfg := config.Default().OIDC
	cfg.Issuer, cfg.ClientID, cfg.ClientSecret = idp.srv.URL, "protodex", "s3cret"
	cfg.RedirectURL = e.srv.URL + CallbackPath
	cfg.GroupRoles = map[string]string{"registry-admins": "admin", "registry-users": "member"}
	if mod != nil {
		mod(&cfg)
	}
	e.h = New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.h.Register(mux)
	jar, _ := cookiejar.New(nil)
	e.cl = &http.Client{Jar: jar}
	return e
}

var tokenRe = regexp.MustCompile(`pdx_[A-Za-z0-9_-]+`)

// login runs the whole flow and returns status and body.
func (e *env) login(t *testing.T) (int, string) {
	t.Helper()
	resp, err := e.cl.Get(e.srv.URL + LoginPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestLoginMintsWorkingToken(t *testing.T) {
	e := setup(t, nil)
	e.idp.claims = map[string]any{"email": "Ann@Example.com", "email_verified": true, "preferred_username": "Ann", "groups": []string{"registry-users", "registry-admins"}}
	code, body := e.login(t)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	secret := tokenRe.FindString(body)
	if secret == "" || !strings.Contains(body, "admin") {
		t.Fatalf("no token / role in page: %s", body)
	}
	a := &auth.Authenticator{Store: e.st}
	id, err := a.Authenticate(context.Background(), secret, "x")
	if err != nil {
		t.Fatal(err)
	}
	if id.Owner.Name != "ann" || id.Owner.Type != "user" || id.Token.ExpiresAt == nil {
		t.Fatalf("identity = %+v", id)
	}
	ident, err := e.st.GetIdentity(context.Background(), e.idp.srv.URL, "sub-1")
	if err != nil || ident.Role != "admin" || ident.OwnerID != id.Owner.ID {
		t.Fatalf("identity row = %+v, %v", ident, err)
	}

	// Second login: same user, new token, role re-derived from claims.
	e.idp.claims["groups"] = []string{"registry-users"}
	code, body = e.login(t)
	if code != 200 {
		t.Fatalf("relogin %d: %s", code, body)
	}
	if s2 := tokenRe.FindString(body); s2 == "" || s2 == secret {
		t.Fatal("expected a fresh token")
	}
	ident, _ = e.st.GetIdentity(context.Background(), e.idp.srv.URL, "sub-1")
	if ident.Role != "member" {
		t.Fatalf("role not refreshed: %+v", ident)
	}
	if n := len(must(e.st.ListTokens(context.Background(), id.Owner.ID))); n != 2 {
		t.Fatalf("tokens = %d", n)
	}
	if n := len(must(e.st.ListOwners(context.Background()))); n != 1 {
		t.Fatalf("owners = %d", n)
	}
}

// must fails the test (by panicking) on error.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestPolicyDenials(t *testing.T) {
	cases := []struct {
		name   string
		mod    func(*config.OIDC)
		claims map[string]any
	}{
		{"no matching group", nil, map[string]any{"email": "a@example.com", "email_verified": true, "groups": []string{"other"}}},
		{"domain not allowed", func(c *config.OIDC) { c.AllowedDomains = []string{"example.com"} },
			map[string]any{"email": "a@evil.org", "email_verified": true, "groups": []string{"registry-users"}}},
		{"unverified email", func(c *config.OIDC) { c.AllowedDomains = []string{"example.com"} },
			map[string]any{"email": "a@example.com", "email_verified": false, "groups": []string{"registry-users"}}},
		{"lookalike domain", func(c *config.OIDC) { c.AllowedDomains = []string{"example.com"} },
			map[string]any{"email": "a@notexample.com", "email_verified": true, "groups": []string{"registry-users"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, tc.mod)
			e.idp.claims = tc.claims
			if code, body := e.login(t); code != http.StatusForbidden || tokenRe.MatchString(body) {
				t.Fatalf("status %d body %s", code, body)
			}
			if len(must(e.st.ListOwners(context.Background()))) != 0 {
				t.Fatal("denied login created a user")
			}
		})
	}
}

func TestDomainAllowedAndDefaultRole(t *testing.T) {
	e := setup(t, func(c *config.OIDC) { c.AllowedDomains = []string{"EXAMPLE.com"}; c.DefaultRole = "viewer" })
	e.idp.claims = map[string]any{"email": "bob@example.com", "email_verified": true}
	if code, body := e.login(t); code != 200 || !strings.Contains(body, "viewer") {
		t.Fatalf("status %d: %s", code, body)
	}
}

func TestNameCollisionDoesNotTakeOverExistingOwner(t *testing.T) {
	e := setup(t, nil)
	ctx := context.Background()
	existing := must(e.st.CreateOwner(ctx, store.Owner{Name: "ann", Type: "user"}))
	e.idp.claims = map[string]any{"preferred_username": "ann", "groups": []string{"registry-users"}}
	if code, body := e.login(t); code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	ident := must(e.st.GetIdentity(ctx, e.idp.srv.URL, "sub-1"))
	if ident.OwnerID == existing.ID {
		t.Fatal("login linked to a pre-existing owner by name")
	}
}

func TestStateAndCookieChecks(t *testing.T) {
	e := setup(t, nil)
	e.idp.claims = map[string]any{"groups": []string{"registry-users"}}
	// No cookie.
	resp, _ := http.Get(e.srv.URL + CallbackPath + "?code=x&state=y")
	if resp.StatusCode != 400 {
		t.Fatalf("no cookie: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	// Start a login but present a different state.
	noRedirect := *e.cl
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r, err := noRedirect.Get(e.srv.URL + LoginPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != 302 || !strings.Contains(r.Header.Get("Location"), "code_challenge=") {
		t.Fatalf("login redirect: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	ck := r.Cookies()
	if len(ck) != 1 || !ck[0].HttpOnly {
		t.Fatalf("cookie = %+v", ck)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+CallbackPath+"?code=x&state=forged", nil)
	req.AddCookie(ck[0])
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Fatalf("forged state: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestTamperedIDTokenRejected(t *testing.T) {
	e := setup(t, nil)
	e.idp.claims = map[string]any{"groups": []string{"registry-admins"}}
	e.idp.mutate = func(s *string) {
		parts := strings.Split(*s, ".")
		parts[2] = strings.Repeat("A", len(parts[2]))
		*s = strings.Join(parts, ".")
	}
	if code, _ := e.login(t); code != http.StatusUnauthorized {
		t.Fatalf("status %d", code)
	}
}

func TestProviderUnavailableIsRetried(t *testing.T) {
	e := setup(t, nil)
	e.h.cfg.Issuer = "http://127.0.0.1:1"
	if code, _ := e.login(t); code != http.StatusBadGateway {
		t.Fatalf("status %d", code)
	}
	e.h.cfg.Issuer = e.idp.srv.URL
	e.idp.claims = map[string]any{"groups": []string{"registry-users"}}
	if code, _ := e.login(t); code != 200 {
		t.Fatalf("no recovery: %d", code)
	}
}

func TestUsernameDerivation(t *testing.T) {
	h := New(config.OIDC{}, nil, nil)
	for _, tc := range []struct {
		claims map[string]any
		email  string
		want   string
	}{
		{map[string]any{"preferred_username": "Jo Smith"}, "", "jo-smith"},
		{map[string]any{"preferred_username": "jo@corp.com"}, "", "jo"},
		{nil, "first.last@corp.com", "first-last"},
		{map[string]any{"preferred_username": "!!!"}, "", "user-"},
	} {
		if got := h.usernameFor(tc.claims, tc.email, "sub"); !strings.HasPrefix(got, tc.want) {
			t.Errorf("usernameFor(%v,%q) = %q, want %q", tc.claims, tc.email, got, tc.want)
		}
	}
	h.cfg.UsernameClaim = "upn"
	if got := h.usernameFor(map[string]any{"upn": "Zed@x.com", "preferred_username": "no"}, "", "s"); got != "zed" {
		t.Errorf("custom claim: %q", got)
	}
}
