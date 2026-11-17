// SPDX-License-Identifier: Apache-2.0

package module_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marijnkoesen/protodex/internal/audit"
	"github.com/marijnkoesen/protodex/internal/module"
	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

func TestPermissionChangesAreAudited(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	acme, err := owner.Create(ctx, st, owner.Organization, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateModule(ctx, store.Module{OwnerID: acme.ID, Name: "api", Visibility: store.VisibilityPrivate, DefaultLabel: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Create(ctx, st, owner.User, "bob"); err != nil {
		t.Fatal(err)
	}

	if _, err := owner.SetRole(ctx, st, "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := module.Grant(ctx, st, "acme", "api", "bob", "write"); err != nil {
		t.Fatal(err)
	}
	if err := module.Revoke(ctx, st, "acme", "api", "bob"); err != nil {
		t.Fatal(err)
	}
	evs, err := st.ListAuditEvents(ctx, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ typ, subject, data string }{
		{audit.UserRoleChanged, "bob", `"from":"member"`},
		{audit.ContributorGranted, "acme/api", `"role":"write"`},
		{audit.ContributorRevoked, "acme/api", `"user":"bob"`},
	}
	if len(evs) != len(want) {
		t.Fatalf("events = %+v", evs)
	}
	for i, w := range want {
		if evs[i].Type != w.typ || evs[i].Subject != w.subject || !strings.Contains(evs[i].Data, w.data) {
			t.Errorf("event %d = %+v, want %+v", i, evs[i], w)
		}
	}
}
