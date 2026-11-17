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
	// Limiter, when set, is consulted with the client address before any
	// work is done (the same hook as bearer-token authentication).
	Limiter auth.Limiter
	// HTTPClient fetches discovery documents and keys. Nil means
	// http.DefaultClient.
	HTTPClient *http.Client
	// Now defaults to time.Now.
	Now func() time.Time

	mu      sync.Mutex
	keySets map[string]oidc.KeySet // by issuer
}

// New returns an Exchanger trusting the issuers in cfg. Discovery and key
// fetching happen on first use.
func New(cfg config.CITrust, st store.Store, log *slog.Logger) *Exchanger {
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = config.Duration(15 * time.Minute)
	}
	return &Exchanger{cfg: cfg, store: st, log: log, keySets: map[string]oidc.KeySet{}}
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
	if e.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, e.HTTPClient)
	}
	return ctx
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
	provider, keys, err := e.issuer(ctx, issuer)
	if err != nil {
		return Response{}, err
	}
	creds, err := e.store.ListModuleTrustCredentials(ctx, m.ID)
	if err != nil {
		return Response{}, err
	}
	var reasons []string
	for _, c := range creds {
		if c.Provider != provider || c.Issuer != issuer {
			continue
		}
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
	if len(reasons) == 0 {
		return Response{}, deny("no trust credential of module %s for issuer %q", req.Module, issuer)
	}
	return Response{}, deny("%s", strings.Join(reasons, "; "))
}

// issuer returns the provider and key set of a trusted issuer.
func (e *Exchanger) issuer(ctx context.Context, iss string) (string, oidc.KeySet, error) {
	provider, jwksURL := "", ""
	if iss == e.cfg.GitHubIssuer {
		provider = store.TrustGitHub
	}
	for _, k := range e.cfg.Kubernetes {
		if iss == k.Issuer {
			provider, jwksURL = store.TrustKubernetes, k.JWKSURL
		}
	}
	if provider == "" {
		return "", nil, deny("untrusted issuer %q", iss)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if ks, ok := e.keySets[iss]; ok {
		return provider, ks, nil
	}
	// The key set outlives this request: only the HTTP client is taken
	// from the context.
	kctx := e.ctx(context.WithoutCancel(ctx))
	if jwksURL == "" {
		p, err := oidc.NewProvider(kctx, iss)
		if err != nil {
			// Not cached: a later request retries discovery.
			return "", nil, fmt.Errorf("discover issuer %q: %w", iss, err)
		}
		var meta struct {
			JWKSURL string `json:"jwks_uri"`
		}
		if err := p.Claims(&meta); err != nil || meta.JWKSURL == "" {
			return "", nil, fmt.Errorf("discover issuer %q: no jwks_uri", iss)
		}
		jwksURL = meta.JWKSURL
	}
	ks := oidc.NewRemoteKeySet(kctx, jwksURL)
	e.keySets[iss] = ks
	return provider, ks, nil
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
