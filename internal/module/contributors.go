// SPDX-License-Identifier: Apache-2.0

package module

import (
	"context"
	"errors"
	"fmt"

	"github.com/marijnkoesen/protodex/internal/store"
)

// ParseContributorRole parses a module contributor role: read, write or admin.
func ParseContributorRole(s string) (string, error) {
	switch s {
	case store.ContributorRead, store.ContributorWrite, store.ContributorAdmin:
		return s, nil
	}
	return "", fmt.Errorf("unknown contributor role %q (want read, write or admin)", s)
}

// Contributor is a user's grant on a module.
type Contributor struct {
	User store.Owner
	Role string
}

func lookup(ctx context.Context, q store.Querier, ownerName, moduleName string) (store.Owner, store.Module, error) {
	o, err := q.GetOwner(ctx, ownerName)
	if err != nil {
		return store.Owner{}, store.Module{}, fmt.Errorf("owner %q: %w", ownerName, err)
	}
	m, err := q.GetModule(ctx, o.ID, moduleName)
	if err != nil {
		return store.Owner{}, store.Module{}, fmt.Errorf("module %s/%s: %w", ownerName, moduleName, err)
	}
	return o, m, nil
}

func lookupUser(ctx context.Context, q store.Querier, name string) (store.Owner, error) {
	u, err := q.GetOwner(ctx, name)
	if err != nil {
		return store.Owner{}, fmt.Errorf("user %q: %w", name, err)
	}
	if u.Type != store.OwnerUser {
		return store.Owner{}, fmt.Errorf("%q is not a user: only users can be contributors", name)
	}
	return u, nil
}

// Grant gives userName role on module ownerName/moduleName, replacing an
// existing grant. Mirror modules are read-only, so they take no grants.
func Grant(ctx context.Context, q store.Querier, ownerName, moduleName, userName, role string) error {
	role, err := ParseContributorRole(role)
	if err != nil {
		return err
	}
	o, m, err := lookup(ctx, q, ownerName, moduleName)
	if err != nil {
		return err
	}
	if o.Mirror {
		return fmt.Errorf("module %s/%s belongs to mirror owner %q and is read-only", ownerName, moduleName, o.Name)
	}
	u, err := lookupUser(ctx, q, userName)
	if err != nil {
		return err
	}
	_, err = q.SetModuleContributor(ctx, store.ModuleContributor{ModuleID: m.ID, UserID: u.ID, Role: role})
	return err
}

// Revoke removes userName's grant on module ownerName/moduleName.
func Revoke(ctx context.Context, q store.Querier, ownerName, moduleName, userName string) error {
	_, m, err := lookup(ctx, q, ownerName, moduleName)
	if err != nil {
		return err
	}
	u, err := lookupUser(ctx, q, userName)
	if err != nil {
		return err
	}
	err = q.DeleteModuleContributor(ctx, m.ID, u.ID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("user %q is not a contributor of %s/%s: %w", userName, ownerName, moduleName, err)
	}
	return err
}

// Contributors lists the grants on module ownerName/moduleName.
func Contributors(ctx context.Context, q store.Querier, ownerName, moduleName string) ([]Contributor, error) {
	_, m, err := lookup(ctx, q, ownerName, moduleName)
	if err != nil {
		return nil, err
	}
	gs, err := q.ListModuleContributors(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]Contributor, 0, len(gs))
	for _, g := range gs {
		u, err := q.GetOwnerByID(ctx, g.UserID)
		if err != nil {
			return nil, err
		}
		out = append(out, Contributor{User: u, Role: g.Role})
	}
	return out, nil
}
