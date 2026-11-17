// SPDX-License-Identifier: Apache-2.0

package oidcauth

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

// TestDexIntegration runs the real flow against a Dex container. Start Dex
// with testdata/dex.yaml on --network host (see .github/workflows/ci.yml) and
// set PROTODEX_TEST_DEX_ISSUER=http://127.0.0.1:5556/dex:
//
//	docker run -d --network host -v $PWD/internal/oidcauth/testdata/dex.yaml:/etc/dex/config.yaml:ro \
//	  ghcr.io/dexidp/dex:v2.41.1 dex serve /etc/dex/config.yaml
func TestDexIntegration(t *testing.T) {
	issuer := os.Getenv("PROTODEX_TEST_DEX_ISSUER")
	if issuer == "" {
		t.Skip("PROTODEX_TEST_DEX_ISSUER not set")
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default().OIDC
	cfg.Issuer, cfg.ClientID, cfg.ClientSecret = issuer, "protodex", "protodex-test-secret"
	cfg.RedirectURL = "http://127.0.0.1:5557/auth/callback" // registered in dex.yaml
	cfg.AllowedDomains = []string{"example.com"}
	cfg.DefaultRole = "member"
	mux := http.NewServeMux()
	New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	ln, err := net.Listen("tcp", "127.0.0.1:5557")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar}
	// Dex redirects /auth to its login form; remember the form URL.
	var form *url.URL
	cl.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.Contains(req.URL.Path, "/auth/local/login") {
			form = req.URL
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := cl.Get(srv.URL + LoginPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if form == nil {
		t.Fatalf("never reached Dex login form (last status %d)", resp.StatusCode)
	}
	cl.CheckRedirect = nil
	resp, err = cl.PostForm(form.String(), url.Values{"login": {"admin@example.com"}, "password": {"password"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("callback status %d: %s", resp.StatusCode, b)
	}
	secret := tokenRe.FindString(string(b))
	if secret == "" {
		t.Fatalf("no token in page: %s", b)
	}
	id, err := (&auth.Authenticator{Store: st}).Authenticate(context.Background(), secret, "x")
	if err != nil {
		t.Fatal(err)
	}
	if id.Owner.Name != "admin" {
		t.Fatalf("owner = %+v", id.Owner)
	}
}
