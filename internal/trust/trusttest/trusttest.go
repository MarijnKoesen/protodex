// SPDX-License-Identifier: Apache-2.0

// Package trusttest provides a fake workload-identity issuer (GitHub
// Actions or Kubernetes style) for tests: OIDC discovery, a JWKS endpoint
// and a signer for identity tokens.
package trusttest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Issuer is a fake token issuer served over HTTP.
type Issuer struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey
}

// NewIssuer starts an issuer; it stops when the test ends.
func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &Issuer{t: t, key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": i.srv.URL, "jwks_uri": i.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"subject_types_supported":               []string{"public"}, "response_types_supported": []string{"id_token"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	i.srv = httptest.NewServer(mux)
	t.Cleanup(i.srv.Close)
	return i
}

// URL is the issuer URL (the iss claim).
func (i *Issuer) URL() string { return i.srv.URL }

// JWKSURL serves the issuer's keys.
func (i *Issuer) JWKSURL() string { return i.srv.URL + "/keys" }

// Token returns a signed identity token for audience with the given extra
// claims (e.g. sub, repository, ref), valid for ttl (negative: expired).
// Claims may override iss, aud and exp.
func (i *Issuer) Token(audience string, ttl time.Duration, claims map[string]any) string {
	i.t.Helper()
	return i.sign(i.key, audience, ttl, claims)
}

// ForeignToken is like Token but signed with a key the issuer does not
// publish, so verification must fail.
func (i *Issuer) ForeignToken(audience string, ttl time.Duration, claims map[string]any) string {
	i.t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		i.t.Fatal(err)
	}
	return i.sign(key, audience, ttl, claims)
}

func (i *Issuer) sign(key *rsa.PrivateKey, audience string, ttl time.Duration, claims map[string]any) string {
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		i.t.Fatal(err)
	}
	now := time.Now()
	std := jwt.Claims{Issuer: i.srv.URL, Audience: jwt.Audience{audience},
		Expiry: jwt.NewNumericDate(now.Add(ttl)), IssuedAt: jwt.NewNumericDate(now.Add(min(ttl, 0) - time.Minute)),
		NotBefore: jwt.NewNumericDate(now.Add(min(ttl, 0) - time.Minute))}
	tok, err := jwt.Signed(sig).Claims(std).Claims(claims).Serialize()
	if err != nil {
		i.t.Fatal(err)
	}
	return tok
}
