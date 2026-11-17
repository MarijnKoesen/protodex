// SPDX-License-Identifier: Apache-2.0

package owner

import (
	"context"
	"fmt"

	"github.com/marijnkoesen/protodex/internal/store"
)

// ParseRole parses an instance role as used on the command line: viewer
// (alias read-only), member or admin.
func ParseRole(s string) (string, error) {
	switch s {
	case store.RoleViewer, "read-only", "readonly":
		return store.RoleViewer, nil
	case store.RoleMember:
		return store.RoleMember, nil
	case store.RoleAdmin:
		return store.RoleAdmin, nil
	}
	return "", fmt.Errorf("unknown role %q (want viewer, member or admin)", s)
}

// SetRole sets the instance role of the user called name. Users who log in
// through OIDC get their role from the identity provider on every login,
// which overrides a role set here.
func SetRole(ctx context.Context, q store.Querier, name, role string) (store.Owner, error) {
	role, err := ParseRole(role)
	if err != nil {
		return store.Owner{}, err
	}
	o, err := q.GetOwner(ctx, name)
	if err != nil {
		return store.Owner{}, fmt.Errorf("user %q: %w", name, err)
	}
	if o.Type != store.OwnerUser {
		return store.Owner{}, fmt.Errorf("%q is %s: only users have roles", name, describe(o))
	}
	if err := q.SetOwnerRole(ctx, o.ID, role); err != nil {
		return store.Owner{}, err
	}
	o.Role = role
	return o, nil
}
