// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

func newStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSecretAndHash(t *testing.T) {
	s1, _ := auth.GenerateSecret()
	s2, _ := auth.GenerateSecret()
	if !strings.HasPrefix(s1, auth.Prefix) || s1 == s2 || !auth.ValidSecretFormat(s1) {
		t.Fatalf("bad secrets %q %q", s1, s2)
	}
	if auth.ValidSecretFormat("pdx_short") || auth.ValidSecretFormat("nope") {
		t.Fatal("accepted malformed secret")
	}
	h := auth.HashSecret(s1)
	if h == s1 || h != auth.HashSecret(s1) || h == auth.HashSecret(s2) || len(h) != 64 {
		t.Fatalf("hash misbehaves: %q", h)
	}
}

func TestBootstrapAndAuthenticate(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	secret, created, err := auth.Bootstrap(ctx, st, "admin", "")
	if err != nil || !created || secret == "" {
		t.Fatalf("bootstrap: %q %v %v", secret, created, err)
	}
	// Only the hash is stored.
	owner, _ := st.GetOwner(ctx, "admin")
	if owner.Role != store.RoleAdmin {
		t.Fatalf("bootstrap user role = %q, want admin", owner.Role)
	}
	toks, _ := st.ListTokens(ctx, owner.ID)
	if len(toks) != 1 || toks[0].Hash == secret || toks[0].Hash != auth.HashSecret(secret) {
		t.Fatalf("stored tokens: %+v", toks)
	}
	if _, again, err := auth.Bootstrap(ctx, st, "other", ""); err != nil || again {
		t.Fatalf("second bootstrap created=%v err=%v", again, err)
	}

	now := time.Now()
	a := &auth.Authenticator{Store: st, Now: func() time.Time { return now }}
	id, err := a.Authenticate(ctx, secret, "c")
	if err != nil || id.Owner.Name != "admin" {
		t.Fatalf("authenticate: %+v %v", id, err)
	}
	if got, _ := st.GetTokenByHash(ctx, auth.HashSecret(secret)); got.LastUsedAt == nil {
		t.Fatal("last used not recorded")
	}
	other, _ := auth.GenerateSecret()
	if _, err := a.Authenticate(ctx, other, "c"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := a.Authenticate(ctx, "garbage", "c"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("malformed token: %v", err)
	}
}

func TestExpiryAndLimiter(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	o, _ := st.CreateOwner(ctx, store.Owner{Name: "u", Type: "user"})
	secret, _ := auth.GenerateSecret()
	exp := time.Now().Add(time.Hour)
	if _, err := st.CreateToken(ctx, store.Token{OwnerID: o.ID, Name: "t", Hash: auth.HashSecret(secret), ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a := &auth.Authenticator{Store: st, Now: func() time.Time { return now }}
	if _, err := a.Authenticate(ctx, secret, "c"); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	now = exp // expiry instant is already expired
	if _, err := a.Authenticate(ctx, secret, "c"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expired token: %v", err)
	}
	a.Limiter = denyAll{}
	if _, err := a.Authenticate(ctx, secret, "c"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("limiter: %v", err)
	}
}

type denyAll struct{}

func (denyAll) Allow(context.Context, string) bool { return false }
