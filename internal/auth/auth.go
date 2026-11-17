// SPDX-License-Identifier: Apache-2.0

// Package auth implements bearer-token credentials: generation, hashing at
// rest, verification and first-run bootstrap.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/marijnkoesen/protodex/internal/store"
)

// Prefix starts every token secret so leaked secrets are greppable and can be
// registered with secret scanners.
const Prefix = "pdx_"

const secretBytes = 32

// ErrInvalidToken means the secret is unknown, malformed or expired. The
// caller must not distinguish these cases to clients.
var ErrInvalidToken = errors.New("auth: invalid or expired token")

// GenerateSecret returns a new random token secret.
func GenerateSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate secret: %w", err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidSecretFormat reports whether s looks like a token secret (prefix and
// at least 32 bytes of encoded entropy).
func ValidSecretFormat(s string) bool {
	rest, ok := strings.CutPrefix(s, Prefix)
	if !ok {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(b) >= secretBytes
}

// HashSecret returns the at-rest representation of a secret. Secrets carry
// 256 bits of entropy, so a fast unsalted hash is appropriate.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Identity is the authenticated principal of a request.
type Identity struct {
	Owner store.Owner
	Token store.Token
}

type identityKey struct{}

// WithIdentity returns ctx carrying id.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext returns the authenticated identity, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Limiter is the rate-limit hook point. Allow is called with the client key
// (peer address) before a presented token is checked; returning false rejects
// the request. A nil Limiter allows everything.
type Limiter interface {
	Allow(ctx context.Context, key string) bool
}

// ErrRateLimited is returned by Authenticate when the Limiter rejects.
var ErrRateLimited = errors.New("auth: rate limited")

// Authenticator resolves bearer secrets to identities.
type Authenticator struct {
	Store   store.Store
	Limiter Limiter
	Now     func() time.Time // defaults to time.Now
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Authenticate verifies secret. clientKey identifies the caller for rate limiting.
func (a *Authenticator) Authenticate(ctx context.Context, secret, clientKey string) (Identity, error) {
	if a.Limiter != nil && !a.Limiter.Allow(ctx, clientKey) {
		return Identity{}, ErrRateLimited
	}
	if !ValidSecretFormat(secret) {
		return Identity{}, ErrInvalidToken
	}
	hash := HashSecret(secret)
	tok, err := a.Store.GetTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Identity{}, ErrInvalidToken
		}
		return Identity{}, err
	}
	if subtle.ConstantTimeCompare([]byte(tok.Hash), []byte(hash)) != 1 {
		return Identity{}, ErrInvalidToken
	}
	now := a.now()
	if tok.ExpiresAt != nil && !now.Before(*tok.ExpiresAt) {
		return Identity{}, ErrInvalidToken
	}
	owner, err := a.Store.GetOwnerByID(ctx, tok.OwnerID)
	if err != nil {
		return Identity{}, err
	}
	// Best effort: a failed bookkeeping write must not fail the request.
	_ = a.Store.TouchToken(ctx, tok.ID, now)
	return Identity{Owner: owner, Token: tok}, nil
}

// Bootstrap creates the first user, an instance admin, and an initial token
// when no user exists.
// If secret is empty a new one is generated. It returns created=false (and no
// secret) when a user already exists. The secret is returned, never logged.
func Bootstrap(ctx context.Context, st store.Store, username, secret string) (out string, created bool, err error) {
	if username == "" {
		return "", false, errors.New("auth: bootstrap: user name is required")
	}
	if secret == "" {
		if secret, err = GenerateSecret(); err != nil {
			return "", false, err
		}
	} else if !ValidSecretFormat(secret) {
		return "", false, fmt.Errorf("auth: bootstrap: token must start with %q followed by at least %d bytes of base64url", Prefix, secretBytes)
	}
	err = st.WithTx(ctx, func(q store.Querier) (err error) {
		created, err = BootstrapIn(ctx, q, username, secret)
		return err
	})
	if err != nil || !created {
		return "", false, err
	}
	return secret, true, nil
}

// BootstrapIn is the transactional core of Bootstrap: within q it creates
// username with a token for secret (which must be valid) unless a user
// already exists. It lets callers bootstrap atomically with other writes.
func BootstrapIn(ctx context.Context, q store.Querier, username, secret string) (created bool, err error) {
	if username == "" {
		return false, errors.New("auth: bootstrap: user name is required")
	}
	if !ValidSecretFormat(secret) {
		return false, errors.New("auth: bootstrap: invalid token")
	}
	owners, err := q.ListOwners(ctx)
	if err != nil {
		return false, err
	}
	for _, o := range owners {
		if o.Type == store.OwnerUser {
			return false, nil // already bootstrapped
		}
	}
	u, err := q.CreateOwner(ctx, store.Owner{Name: username, Type: store.OwnerUser, Role: store.RoleAdmin})
	if err != nil {
		return false, err
	}
	if _, err := q.CreateToken(ctx, store.Token{OwnerID: u.ID, Name: "bootstrap", Hash: HashSecret(secret)}); err != nil {
		return false, err
	}
	return true, nil
}
