// SPDX-License-Identifier: Apache-2.0

package trust

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/store"
)

// ExchangePath is the token exchange endpoint.
const ExchangePath = "/auth/ci-token"

// maxRequestBytes bounds an exchange request; identity tokens are a few KiB.
const maxRequestBytes = 64 << 10

// Default limits of the exchange endpoint per client address: a burst of
// ExchangeBurst requests (a CI matrix fanning out), refilled at
// ExchangeRate per second.
const (
	ExchangeRate  = 1.0
	ExchangeBurst = 30
)

// discoveryTimeout bounds fetching an issuer's discovery document and keys,
// so a stalled issuer cannot hold requests indefinitely.
const discoveryTimeout = 10 * time.Second

// signingAlgs are the accepted identity-token signature algorithms
// (asymmetric only).
var signingAlgs = []string{oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512, oidc.PS256, oidc.PS384, oidc.PS512}

// ErrDenied means the exchange was refused. Clients only ever learn this,
// never which check failed.
var ErrDenied = errors.New("trust: token exchange denied")

// denied wraps ErrDenied with the reason, which is logged server-side.
type denied struct{ reason string }

func (d denied) Error() string { return "trust: token exchange denied: " + d.reason }
func (d denied) Unwrap() error { return ErrDenied }

func deny(format string, args ...any) error { return denied{reason: fmt.Sprintf(format, args...)} }

// Exchanger exchanges CI identity tokens for scoped registry tokens.
type Exchanger struct {
	cfg   config.CITrust
	store store.Store
	log   *slog.Logger
	// Limiter is consulted with the client address before any work is
	// done. New installs a per-client token bucket (ExchangeRate,
	// ExchangeBurst); nil disables rate limiting.
	Limiter auth.Limiter
	// HTTPClient fetches discovery documents and keys. Nil means a client
	// with a discoveryTimeout timeout.
	HTTPClient *http.Client
	// Now defaults to time.Now.
	Now func() time.Time

	mu      sync.Mutex              // guards issuers only, never held during I/O
	issuers map[string]*issuerEntry // by issuer URL
}

// issuerEntry caches the key set of one issuer. sem (capacity 1) serializes
// discovery of that issuer only, so a stalled issuer never blocks others.
type issuerEntry struct {
	sem  chan struct{}
	keys oidc.KeySet // set once discovered; guarded by sem
}

// defaultHTTPClient is used without Exchanger.HTTPClient.
var defaultHTTPClient = &http.Client{Timeout: discoveryTimeout}

// New returns an Exchanger trusting the issuers in cfg. Discovery and key
// fetching happen on first use.
func New(cfg config.CITrust, st store.Store, log *slog.Logger) *Exchanger {
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = config.Duration(15 * time.Minute)
	}
	return &Exchanger{cfg: cfg, store: st, log: log, issuers: map[string]*issuerEntry{},
		Limiter: auth.NewRateLimiter(ExchangeRate, ExchangeBurst)}
}

// Register mounts the exchange endpoint on mux.
func (e *Exchanger) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+ExchangePath, e.serveHTTP)
}

func (e *Exchanger) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Exchanger) ctx(ctx context.Context) context.Context {
	c := e.HTTPClient
	if c == nil {
		c = defaultHTTPClient
	}
	return oidc.ClientContext(ctx, c)
}

// Request is the JSON body of an exchange.
type Request struct {
	// Token is the CI platform's identity token (a JWT).
	Token string `json:"token"`
	// Module is "OWNER/MODULE", the module to get a token for.
	Module string `json:"module"`
}

// Response is the JSON answer to a successful exchange.
type Response struct {
	// Token is the registry token secret (use as BUF_TOKEN).
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Module    string    `json:"module"`
	Role      string    `json:"role"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (e *Exchanger) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if e.Limiter != nil && !e.Limiter.Allow(r.Context(), r.RemoteAddr) {
		writeError(w, http.StatusTooManyRequests, "rate limited")
		return
	}
	var req Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Token == "" || req.Module == "" {
		writeError(w, http.StatusBadRequest, `want a JSON body {"token": "<identity token>", "module": "OWNER/MODULE"}`)
		return
	}
	resp, err := e.Exchange(r.Context(), req)
	switch {
	case errors.Is(err, ErrDenied):
		e.log.Warn("ci token exchange denied", "module", req.Module, "remote", r.RemoteAddr, "reason", err)
		writeError(w, http.StatusUnauthorized, "token exchange denied")
		return
	case err != nil:
		e.log.Error("ci token exchange failed", "module", req.Module, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Exchange verifies req.Token against the trust credentials of req.Module
// and mints a scoped token for the first credential whose rules match.
// Every refusal wraps ErrDenied.
func (e *Exchanger) Exchange(ctx context.Context, req Request) (Response, error) {
	ownerName, moduleName, err := splitModule(req.Module)
	if err != nil {
		return Response{}, deny("%v", err)
	}
	o, err := e.store.GetOwner(ctx, ownerName)
	if errors.Is(err, store.ErrNotFound) {
		return Response{}, deny("unknown owner %q", ownerName)
	} else if err != nil {
		return Response{}, err
	}
	m, err := e.store.GetModule(ctx, o.ID, moduleName)
	if errors.Is(err, store.ErrNotFound) {
		return Response{}, deny("unknown module %q", req.Module)
	} else if err != nil {
		return Response{}, err
	}
	issuer, err := unverifiedIssuer(req.Token)
	if err != nil {
		return Response{}, deny("malformed token: %v", err)
	}
	provider, jwksURL, err := e.trusted(issuer)
	if err != nil {
		return Response{}, err
	}
	all, err := e.store.ListModuleTrustCredentials(ctx, m.ID)
	if err != nil {
		return Response{}, err
	}
	var creds []store.TrustCredential
	for _, c := range all {
		if c.Provider == provider && c.Issuer == issuer {
			creds = append(creds, c)
		}
	}
	// Only contact the issuer when a credential could accept its token.
	if len(creds) == 0 {
		return Response{}, deny("no trust credential of module %s for issuer %q", req.Module, issuer)
	}
	keys, err := e.keySet(ctx, issuer, jwksURL)
	if err != nil {
		return Response{}, err
	}
	var reasons []string
	for _, c := range creds {
		cl, err := e.verify(ctx, keys, c, req.Token)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", c.Name, err))
			continue
		}
		if ok, failed := Match(c, cl); !ok {
			reasons = append(reasons, fmt.Sprintf("%s: %s rule did not match (sub %q, repository %q, ref %q)", c.Name, failed, cl.Subject, cl.Repository, cl.Ref))
			continue
		}
		resp, err := e.mint(ctx, c, req.Module)
		if err == nil {
			e.log.Info("ci token issued", "credential", c.Name, "module", req.Module, "sub", cl.Subject,
				"repository", cl.Repository, "ref", cl.Ref, "role", c.Role, "expires_at", resp.ExpiresAt)
		}
		return resp, err
	}
	return Response{}, deny("%s", strings.Join(reasons, "; "))
}

// trusted returns the provider of a configured issuer and, for
// Kubernetes issuers, their JWKS URL.
func (e *Exchanger) trusted(iss string) (provider, jwksURL string, err error) {
	if iss == e.cfg.GitHubIssuer {
		provider = store.TrustGitHub
	}
	for _, k := range e.cfg.Kubernetes {
		if iss == k.Issuer {
			provider, jwksURL = store.TrustKubernetes, k.JWKSURL
		}
	}
	if provider == "" {
		return "", "", deny("untrusted issuer %q", iss)
	}
	return provider, jwksURL, nil
}

// keySet returns the (cached) key set of a trusted issuer, discovering its
// JWKS URL first when jwksURL is empty. Discovery is bounded by
// discoveryTimeout and by ctx, and only blocks requests for the same
// issuer; a failure is not cached, so a later request retries.
func (e *Exchanger) keySet(ctx context.Context, iss, jwksURL string) (oidc.KeySet, error) {
	e.mu.Lock()
	ent, ok := e.issuers[iss]
	if !ok {
		ent = &issuerEntry{sem: make(chan struct{}, 1)}
		e.issuers[iss] = ent
	}
	e.mu.Unlock()

	select {
	case ent.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("discover issuer %q: %w", iss, ctx.Err())
	}
	defer func() { <-ent.sem }()
	if ent.keys != nil {
		return ent.keys, nil
	}
	if jwksURL == "" {
		dctx, cancel := context.WithTimeout(e.ctx(ctx), discoveryTimeout)
		defer cancel()
		p, err := oidc.NewProvider(dctx, iss)
		if err != nil {
			return nil, fmt.Errorf("discover issuer %q: %w", iss, err)
		}
		var meta struct {
			JWKSURL string `json:"jwks_uri"`
		}
		if err := p.Claims(&meta); err != nil || meta.JWKSURL == "" {
			return nil, fmt.Errorf("discover issuer %q: no jwks_uri", iss)
		}
		jwksURL = meta.JWKSURL
	}
	// The key set outlives this request: it keeps only the HTTP client of
	// the context (whose timeout bounds key fetches), not its deadline.
	ent.keys = oidc.NewRemoteKeySet(e.ctx(context.WithoutCancel(ctx)), jwksURL)
	return ent.keys, nil
}

// verify checks the signature, issuer, audience and expiry of rawToken
// for credential c and returns its claims.
func (e *Exchanger) verify(ctx context.Context, keys oidc.KeySet, c store.TrustCredential, rawToken string) (Claims, error) {
	v := oidc.NewVerifier(c.Issuer, keys, &oidc.Config{ClientID: c.Audience, SupportedSigningAlgs: signingAlgs, Now: e.now})
	idt, err := v.Verify(e.ctx(ctx), rawToken)
	if err != nil {
		return Claims{}, err
	}
	var cl Claims
	if err := idt.Claims(&cl); err != nil {
		return Claims{}, err
	}
	return cl, nil
}

// mint creates a token of c's user restricted to c's module and role.
func (e *Exchanger) mint(ctx context.Context, c store.TrustCredential, module string) (Response, error) {
	ttl := c.TokenTTL
	if ttl <= 0 {
		ttl = time.Duration(e.cfg.TokenTTL)
	}
	ttl = min(ttl, config.MaxCITokenTTL)
	secret, err := auth.GenerateSecret()
	if err != nil {
		return Response{}, err
	}
	now := e.now()
	exp := now.Add(ttl)
	err = e.store.WithTx(ctx, func(q store.Querier) error {
		u, err := q.GetOwnerByID(ctx, c.UserID)
		if err != nil {
			return err
		}
		if u.Type != store.OwnerUser {
			return deny("credential %s acts as %q, which is not a user", c.Name, u.Name)
		}
		// Housekeeping: expired CI tokens are useless, keep the table small.
		if _, err := q.DeleteExpiredScopedTokens(ctx, now); err != nil {
			return err
		}
		_, err = q.CreateToken(ctx, store.Token{OwnerID: c.UserID, Name: "ci:" + c.Name, Hash: auth.HashSecret(secret),
			CreatedAt: now, ExpiresAt: &exp, ModuleID: c.ModuleID, ModuleRole: c.Role})
		return err
	})
	if err != nil {
		return Response{}, err
	}
	return Response{Token: secret, ExpiresAt: exp.UTC().Truncate(time.Second), Module: module, Role: c.Role}, nil
}

// unverifiedIssuer reads the iss claim of a JWT without verifying it; it
// only selects which keys to verify the token with.
func unverifiedIssuer(raw string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", errors.New("not a JWT")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("payload is not base64url")
	}
	var cl struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(b, &cl); err != nil || cl.Issuer == "" {
		return "", errors.New("no iss claim")
	}
	return cl.Issuer, nil
}
