// SPDX-License-Identifier: Apache-2.0

// Package oidcauth implements user login through an OpenID Connect provider
// (authorization-code flow with PKCE). A successful login provisions or
// updates the local user, maps IdP groups to a role and mints a personal
// access token that is shown once and then used by the CLI as a bearer token.
package oidcauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/store"
)

const (
	// LoginPath starts a login.
	LoginPath = "/auth/login"
	// CallbackPath must match the configured redirect URL.
	CallbackPath = "/auth/callback"

	cookieName = "protodex_oidc"
	cookieTTL  = 10 * time.Minute
)

// Handler serves the OIDC login endpoints.
type Handler struct {
	cfg   config.OIDC
	store store.Store
	log   *slog.Logger
	// HTTPClient is used for discovery, key fetching and the code exchange.
	// Nil means http.DefaultClient.
	HTTPClient *http.Client
	Now        func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider
}

// New returns a Handler. Provider discovery is deferred to first use so an
// unavailable IdP does not keep the registry from starting.
func New(cfg config.OIDC, st store.Store, log *slog.Logger) *Handler {
	return &Handler{cfg: cfg, store: st, log: log}
}

// Register mounts the login endpoints on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+LoginPath, h.login)
	mux.HandleFunc("GET "+CallbackPath, h.callback)
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) ctx(r *http.Request) context.Context {
	ctx := r.Context()
	if h.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, h.HTTPClient)
	}
	return ctx
}

func (h *Handler) discover(ctx context.Context) (*oidc.Provider, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.provider != nil {
		return h.provider, nil
	}
	p, err := oidc.NewProvider(ctx, h.cfg.Issuer)
	if err != nil {
		return nil, err
	}
	h.provider = p
	return p, nil
}

func (h *Handler) oauth(p *oidc.Provider) *oauth2.Config {
	scopes := []string{oidc.ScopeOpenID}
	for _, s := range h.cfg.Scopes {
		if s != oidc.ScopeOpenID {
			scopes = append(scopes, s)
		}
	}
	return &oauth2.Config{
		ClientID:     h.cfg.ClientID,
		ClientSecret: h.cfg.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  h.cfg.RedirectURL,
		Scopes:       scopes,
	}
}

func random() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (h *Handler) secureCookie() bool { return strings.HasPrefix(h.cfg.RedirectURL, "https://") }

func (h *Handler) fail(w http.ResponseWriter, status int, public string, err error) {
	if err != nil {
		h.log.Warn("oidc login failed", "reason", public, "error", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, public, status)
}

// login redirects to the provider. State, nonce and the PKCE verifier are kept
// in a short-lived HttpOnly cookie bound to this browser.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	p, err := h.discover(h.ctx(r))
	if err != nil {
		h.fail(w, http.StatusBadGateway, "identity provider unavailable", err)
		return
	}
	state, err1 := random()
	nonce, err2 := random()
	verifier := oauth2.GenerateVerifier()
	if err := errors.Join(err1, err2); err != nil {
		h.fail(w, http.StatusInternalServerError, "internal error", err)
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the https redirect URL; plain http is for local dev only
		Name: cookieName, Value: state + "." + nonce + "." + verifier, Path: CallbackPath,
		MaxAge: int(cookieTTL.Seconds()), HttpOnly: true, Secure: h.secureCookie(), SameSite: http.SameSiteLaxMode,
	})
	u := h.oauth(p).AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u, http.StatusFound)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(cookieName)
	// Always clear the one-shot cookie.
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: CallbackPath, MaxAge: -1, HttpOnly: true, Secure: h.secureCookie(), SameSite: http.SameSiteLaxMode}) //nolint:gosec // see login
	if err != nil {
		h.fail(w, http.StatusBadRequest, "login session missing or expired; start again at "+LoginPath, nil)
		return
	}
	parts := strings.Split(c.Value, ".")
	q := r.URL.Query()
	if len(parts) != 3 || subtle.ConstantTimeCompare([]byte(parts[0]), []byte(q.Get("state"))) != 1 {
		h.fail(w, http.StatusBadRequest, "invalid state", nil)
		return
	}
	nonce, verifier := parts[1], parts[2]
	if e := q.Get("error"); e != "" {
		h.fail(w, http.StatusUnauthorized, "login was not completed by the identity provider", fmt.Errorf("provider error %q: %s", e, q.Get("error_description")))
		return
	}
	code := q.Get("code")
	if code == "" {
		h.fail(w, http.StatusBadRequest, "missing authorization code", nil)
		return
	}

	ctx := h.ctx(r)
	p, err := h.discover(ctx)
	if err != nil {
		h.fail(w, http.StatusBadGateway, "identity provider unavailable", err)
		return
	}
	oc := h.oauth(p)
	tok, err := oc.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		h.fail(w, http.StatusUnauthorized, "code exchange failed", err)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		h.fail(w, http.StatusUnauthorized, "provider returned no id_token", nil)
		return
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: h.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		h.fail(w, http.StatusUnauthorized, "invalid id_token", err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		h.fail(w, http.StatusUnauthorized, "nonce mismatch", nil)
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		h.fail(w, http.StatusUnauthorized, "unreadable id_token claims", err)
		return
	}
	// Some providers only put profile claims on userinfo; fill gaps from it.
	if _, ok := claims["email"]; !ok {
		if ui, err := p.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil && ui.Subject == idt.Subject {
			var extra map[string]any
			if ui.Claims(&extra) == nil {
				for k, v := range extra {
					if _, have := claims[k]; !have {
						claims[k] = v
					}
				}
			}
		}
	}

	who, err := h.Authorize(idt.Issuer, idt.Subject, claims)
	if err != nil {
		var de *DeniedError
		if errors.As(err, &de) {
			h.fail(w, http.StatusForbidden, de.Public, err)
			return
		}
		h.fail(w, http.StatusInternalServerError, "internal error", err)
		return
	}
	secret, owner, exp, err := h.provision(r.Context(), who)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "internal error", err)
		return
	}
	h.log.Info("oidc login", "user", owner.Name, "role", who.Role, "issuer", who.Issuer)

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_ = tokenPage.Execute(w, map[string]any{"User": owner.Name, "Role": who.Role, "Token": secret, "Expires": exp.UTC().Format(time.RFC3339)})
}

var tokenPage = template.Must(template.New("token").Parse(`<!doctype html>
<meta charset="utf-8"><title>Protodex login</title>
<style>body{font:16px system-ui;max-width:42em;margin:3em auto;padding:0 1em}code{display:block;padding:.8em;background:#eee;word-break:break-all}</style>
<h1>Signed in as {{.User}}</h1>
<p>Role: <b>{{.Role}}</b>. Your personal access token (shown once, expires {{.Expires}}):</p>
<code>{{.Token}}</code>
<p>Use it with the CLI, e.g. <code>buf registry login</code>, or as <code>Authorization: Bearer …</code>.
Manage tokens afterwards with the TokenService API.</p>`))

// Who is a verified, authorized login.
type Who struct {
	Issuer, Subject, Email, Username, Role string
}

// DeniedError means the user authenticated but may not log in.
type DeniedError struct{ Public, Reason string }

func (e *DeniedError) Error() string { return "oidc: login denied: " + e.Reason }

// Authorize applies the allowed-domain policy and group→role mapping to
// verified ID token claims.
func (h *Handler) Authorize(issuer, subject string, claims map[string]any) (Who, error) {
	email, _ := claims["email"].(string)
	verified, _ := claims["email_verified"].(bool)
	if len(h.cfg.AllowedDomains) > 0 {
		at := strings.LastIndexByte(email, '@')
		if email == "" || at < 0 || !verified {
			return Who{}, &DeniedError{"a verified e-mail address is required", "no verified email"}
		}
		domain := strings.ToLower(email[at+1:])
		if !hasFold(h.cfg.AllowedDomains, domain) {
			return Who{}, &DeniedError{"your e-mail domain is not allowed", "domain " + domain}
		}
	}
	role := h.roleFor(claims)
	if role == "" {
		return Who{}, &DeniedError{"your account has no access to this registry", "no matching group"}
	}
	return Who{Issuer: issuer, Subject: subject, Email: email, Username: h.usernameFor(claims, email, subject), Role: role}, nil
}

func hasFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), s) {
			return true
		}
	}
	return false
}

func (h *Handler) roleFor(claims map[string]any) string {
	best := ""
	claim := h.cfg.GroupsClaim
	if claim == "" {
		claim = "groups"
	}
	var groups []string
	switch v := claims[claim].(type) {
	case []any:
		for _, g := range v {
			if s, ok := g.(string); ok {
				groups = append(groups, s)
			}
		}
	case string:
		groups = strings.Fields(strings.ReplaceAll(v, ",", " "))
	}
	for _, g := range groups {
		if r, ok := h.cfg.GroupRoles[g]; ok && config.RoleRank(r) > config.RoleRank(best) {
			best = r
		}
	}
	if best == "" {
		best = h.cfg.DefaultRole
	}
	return best
}

var invalidName = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitizeName turns s into a lower-case [a-z0-9-] owner name.
func sanitizeName(s string) string {
	s = invalidName.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	return s
}

func (h *Handler) usernameFor(claims map[string]any, email, subject string) string {
	var cands []string
	if c := h.cfg.UsernameClaim; c != "" {
		if s, ok := claims[c].(string); ok {
			cands = append(cands, s)
		}
	} else if s, ok := claims["preferred_username"].(string); ok {
		cands = append(cands, s)
	}
	local, _, _ := strings.Cut(email, "@")
	cands = append(cands, local)
	for _, c := range cands {
		// A preferred_username may itself be an e-mail address.
		c, _, _ = strings.Cut(c, "@")
		if n := sanitizeName(c); n != "" {
			return n
		}
	}
	sum := sha256.Sum256([]byte(subject))
	return "user-" + base64.RawURLEncoding.EncodeToString(sum[:4])
}

// provision finds or creates the user for who, refreshes its role and mints a
// personal access token.
func (h *Handler) provision(ctx context.Context, who Who) (secret string, owner store.Owner, exp time.Time, err error) {
	ttl := time.Duration(h.cfg.TokenTTL)
	if ttl <= 0 {
		ttl = 720 * time.Hour
	}
	now := h.now()
	exp = now.Add(ttl)
	secret, err = auth.GenerateSecret()
	if err != nil {
		return "", store.Owner{}, exp, err
	}
	err = h.store.WithTx(ctx, func(q store.Querier) error {
		ident, err := q.GetIdentity(ctx, who.Issuer, who.Subject)
		switch {
		case err == nil:
			ident.Email, ident.Role, ident.LastLoginAt = who.Email, who.Role, now
			if err := q.UpdateIdentity(ctx, ident); err != nil {
				return err
			}
			if owner, err = auth.OwnerByID(ctx, q, ident.OwnerID); err != nil {
				return err
			}
		case errors.Is(err, store.ErrNotFound):
			if owner, err = h.createUser(ctx, q, who.Username); err != nil {
				return err
			}
			if _, err = q.CreateIdentity(ctx, store.Identity{Issuer: who.Issuer, Subject: who.Subject, OwnerID: owner.ID,
				Email: who.Email, Role: who.Role, CreatedAt: now, LastLoginAt: now}); err != nil {
				return err
			}
		default:
			return err
		}
		_, err = q.CreateToken(ctx, store.Token{OwnerID: owner.ID, Name: "oidc-login " + now.UTC().Format("2006-01-02 15:04"),
			Hash: auth.HashSecret(secret), ExpiresAt: &exp})
		return err
	})
	return secret, owner, exp, err
}

// createUser creates a user owner, suffixing the name on conflict. Existing
// owners are never linked to by name: a new login must not take over another
// account.
func (h *Handler) createUser(ctx context.Context, q store.Querier, base string) (store.Owner, error) {
	for i := range 8 {
		name := base
		if i > 0 {
			var b [3]byte
			if _, err := rand.Read(b[:]); err != nil {
				return store.Owner{}, err
			}
			name = base + "-" + hex.EncodeToString(b[:])
		}
		o, err := q.CreateOwner(ctx, store.Owner{Name: name, Type: "user"})
		if !errors.Is(err, store.ErrConflict) {
			return o, err
		}
	}
	return store.Owner{}, errors.New("oidc: could not allocate a unique user name")
}
