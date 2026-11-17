// SPDX-License-Identifier: Apache-2.0

package oidcauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeIDP is a minimal OIDC provider: discovery, JWKS, authorize (auto-approve)
// and token endpoints. It enforces PKCE and returns signed ID tokens.
type fakeIDP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	claims map[string]any // extra claims for the next ID token
	mu     sync.Mutex
	codes  map[string]authReq
	// mutate lets tests tamper with the token response.
	mutate func(idToken *string)
	// userinfo, when set, is served at /userinfo.
	userinfo map[string]any
}

type authReq struct {
	nonce, challenge, clientID string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIDP{t: t, key: key, codes: map[string]authReq{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/keys", "userinfo_endpoint": f.srv.URL + "/userinfo", "id_token_signing_alg_values_supported": []string{"RS256"},
			"subject_types_supported": []string{"public"}, "response_types_supported": []string{"code"},
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.userinfo)
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := "code-" + q.Get("nonce")[:8]
		f.mu.Lock()
		f.codes[code] = authReq{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), clientID: q.Get("client_id")}
		f.mu.Unlock()
		if q.Get("code_challenge_method") != "S256" {
			f.t.Error("PKCE S256 not requested")
		}
		redir, _ := url.Parse(q.Get("redirect_uri"))
		rq := redir.Query()
		rq.Set("code", code)
		rq.Set("state", q.Get("state"))
		redir.RawQuery = rq.Encode()
		http.Redirect(w, r, redir.String(), http.StatusFound) //nolint:gosec // test fake
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		ar, ok := f.codes[r.Form.Get("code")]
		delete(f.codes, r.Form.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != ar.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		sig, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		std := jwt.Claims{Issuer: f.srv.URL, Subject: "sub-1", Audience: jwt.Audience{ar.clientID},
			Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)), IssuedAt: jwt.NewNumericDate(time.Now())}
		b := jwt.Signed(sig).Claims(std).Claims(map[string]any{"nonce": ar.nonce}).Claims(f.claims)
		idt, err := b.Serialize()
		if err != nil {
			f.t.Error(err)
		}
		if f.mutate != nil {
			f.mutate(&idt)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idt})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}
