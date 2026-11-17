// SPDX-License-Identifier: Apache-2.0

package owner_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

func newStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestValidateName(t *testing.T) {
	for _, tc := range []struct {
		kind owner.Kind
		name string
		ok   bool
	}{
		{owner.User, "alice", true},
		{owner.User, "a", true},
		{owner.User, "a1-b2", true},
		{owner.User, strings.Repeat("a", 32), true},
		{owner.User, strings.Repeat("a", 33), false},
		{owner.Organization, strings.Repeat("a", 48), true},
		{owner.Mirror, strings.Repeat("a", 49), false},
		{owner.User, "", false},
		{owner.User, "Alice", false},
		{owner.User, "1abc", false},
		{owner.User, "abc-", false},
		{owner.User, "a_b", false},
		{owner.User, "a.b", false},
	} {
		err := owner.ValidateName(tc.kind, tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateName(%s, %q) = %v, want ok=%v", tc.kind, tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, owner.ErrInvalidName) {
			t.Errorf("error %v is not ErrInvalidName", err)
		}
	}
}

func TestParseKind(t *testing.T) {
	for in, want := range map[string]owner.Kind{"user": owner.User, "org": owner.Organization, "organization": owner.Organization, "mirror": owner.Mirror} {
		if got, err := owner.ParseKind(in); err != nil || got != want {
			t.Errorf("ParseKind(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := owner.ParseKind("team"); err == nil {
		t.Error("ParseKind(team) succeeded")
	}
}

func TestCreateEnsureRename(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	u, err := owner.Create(ctx, st, owner.User, "alice")
	if err != nil || u.Type != store.OwnerUser || u.Mirror {
		t.Fatalf("create user: %+v %v", u, err)
	}
	m, err := owner.Create(ctx, st, owner.Mirror, "googleapis")
	if err != nil || m.Type != store.OwnerOrganization || !m.Mirror || owner.KindOf(m) != owner.Mirror {
		t.Fatalf("create mirror: %+v %v", m, err)
	}
	if _, err := owner.Create(ctx, st, owner.User, "Bad"); !errors.Is(err, owner.ErrInvalidName) {
		t.Fatalf("invalid name: %v", err)
	}
	// A mirror reserves its name for every kind.
	_, err = owner.Create(ctx, st, owner.Organization, "googleapis")
	if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "mirror owner") {
		t.Fatalf("shadowing a mirror: %v", err)
	}

	// Ensure is idempotent for the same kind and refuses another kind.
	org, created, err := owner.Ensure(ctx, st, owner.Organization, "acme")
	if err != nil || !created || owner.KindOf(org) != owner.Organization {
		t.Fatalf("ensure org: %+v %v %v", org, created, err)
	}
	again, created, err := owner.Ensure(ctx, st, owner.Organization, "acme")
	if err != nil || created || again.ID != org.ID {
		t.Fatalf("ensure org again: %+v %v %v", again, created, err)
	}
	if _, _, err := owner.Ensure(ctx, st, owner.Mirror, "acme"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ensure mirror over org: %v", err)
	}
	if _, _, err := owner.Ensure(ctx, st, owner.Organization, "googleapis"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ensure org over mirror: %v", err)
	}

	// Rename rules.
	r, err := owner.Rename(ctx, st, "acme", "acme-corp")
	if err != nil || r.ID != org.ID || r.Name != "acme-corp" {
		t.Fatalf("rename: %+v %v", r, err)
	}
	if _, err := owner.Rename(ctx, st, "acme", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rename old name: %v", err)
	}
	if _, err := owner.Rename(ctx, st, "acme-corp", "alice"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("rename onto user: %v", err)
	}
	if _, err := owner.Rename(ctx, st, "googleapis", "gapis"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("rename mirror: %v", err)
	}
	// User names are capped at 32 characters, organization names at 48.
	if _, err := owner.Rename(ctx, st, "alice", strings.Repeat("a", 40)); !errors.Is(err, owner.ErrInvalidName) {
		t.Fatalf("long user name: %v", err)
	}
	if _, err := owner.Rename(ctx, st, "acme-corp", strings.Repeat("a", 40)); err != nil {
		t.Fatalf("long org name: %v", err)
	}
}
