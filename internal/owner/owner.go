// SPDX-License-Identifier: Apache-2.0

// Package owner holds the owner naming rules and the administrative
// operations (create, ensure, rename) behind the `protodex owner` and
// `protodex bootstrap` commands.
//
// Protodex uses a single-organization model: an instance normally has one
// organization plus its users. The owners table still holds any number of
// owners, so extra organizations (teams, namespaces) and mirror owners such as
// googleapis can coexist. All owner names share one namespace, so a mirror
// owner reserves its upstream name and no local user or organization can
// shadow it.
package owner

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/marijnkoesen/protodex/internal/store"
)

// Kind is the kind of owner an administrator creates.
type Kind string

// Owner kinds.
const (
	User         Kind = "user"
	Organization Kind = "org"
	Mirror       Kind = "mirror"
)

// ParseKind parses a kind name as used on the command line.
func ParseKind(s string) (Kind, error) {
	switch s {
	case "user":
		return User, nil
	case "org", "organization":
		return Organization, nil
	case "mirror":
		return Mirror, nil
	}
	return "", fmt.Errorf("unknown owner kind %q (want user, org or mirror)", s)
}

// KindOf reports the kind of an existing owner.
func KindOf(o store.Owner) Kind {
	switch {
	case o.Type == store.OwnerUser:
		return User
	case o.Mirror:
		return Mirror
	}
	return Organization
}

// Name length limits, from the registry API's validation rules.
const (
	MaxUserNameLen = 32
	MaxOrgNameLen  = 48
)

var namePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// ErrInvalidName means a name breaks the owner naming rules.
var ErrInvalidName = errors.New("owner: invalid name")

// ValidateName checks name against the rules for kind: lowercase letters,
// digits and inner hyphens, starting with a letter; at most 32 characters for
// users and 48 for organizations and mirrors.
func ValidateName(kind Kind, name string) error {
	limit := MaxOrgNameLen
	if kind == User {
		limit = MaxUserNameLen
	}
	if len(name) == 0 || len(name) > limit {
		return fmt.Errorf("%w %q: must be 1 to %d characters", ErrInvalidName, name, limit)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w %q: use lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen", ErrInvalidName, name)
	}
	return nil
}

func record(kind Kind, name string) store.Owner {
	o := store.Owner{Name: name, Type: store.OwnerOrganization}
	switch kind {
	case User:
		o.Type = store.OwnerUser
	case Mirror:
		o.Mirror = true
	}
	return o
}

// Create creates a new owner. A taken name returns store.ErrConflict.
func Create(ctx context.Context, q store.Querier, kind Kind, name string) (store.Owner, error) {
	if _, err := ParseKind(string(kind)); err != nil {
		return store.Owner{}, err
	}
	if err := ValidateName(kind, name); err != nil {
		return store.Owner{}, err
	}
	o, err := q.CreateOwner(ctx, record(kind, name))
	if errors.Is(err, store.ErrConflict) {
		return store.Owner{}, nameTaken(ctx, q, name, err)
	}
	return o, err
}

func nameTaken(ctx context.Context, q store.Querier, name string, err error) error {
	if existing, gerr := q.GetOwner(ctx, name); gerr == nil {
		return fmt.Errorf("%w: name %q is already used by %s", store.ErrConflict, name, describe(existing))
	}
	return err
}

func describe(o store.Owner) string {
	switch KindOf(o) {
	case User:
		return "a user"
	case Mirror:
		return "a mirror owner"
	}
	return "an organization"
}

// Ensure creates the owner unless one of the same kind already exists under
// name. It is idempotent; an owner of another kind holding the name is an
// error (store.ErrConflict).
func Ensure(ctx context.Context, q store.Querier, kind Kind, name string) (o store.Owner, created bool, err error) {
	if err := ValidateName(kind, name); err != nil {
		return store.Owner{}, false, err
	}
	existing, err := q.GetOwner(ctx, name)
	switch {
	case err == nil:
		if KindOf(existing) != kind {
			return store.Owner{}, false, fmt.Errorf("%w: name %q is already used by %s", store.ErrConflict, name, describe(existing))
		}
		return existing, false, nil
	case !errors.Is(err, store.ErrNotFound):
		return store.Owner{}, false, err
	}
	o, err = Create(ctx, q, kind, name)
	return o, err == nil, err
}

// Rename renames the owner called oldName. The new name must follow the
// rules for the owner's kind and be free; mirror owners keep their upstream
// name and cannot be renamed.
func Rename(ctx context.Context, q store.Querier, oldName, newName string) (store.Owner, error) {
	o, err := q.GetOwner(ctx, oldName)
	if err != nil {
		return store.Owner{}, err
	}
	if o.Mirror {
		return store.Owner{}, fmt.Errorf("%w: mirror owner %q keeps its upstream name and cannot be renamed", store.ErrConflict, oldName)
	}
	if err := ValidateName(KindOf(o), newName); err != nil {
		return store.Owner{}, err
	}
	r, err := q.RenameOwner(ctx, o.ID, newName)
	if errors.Is(err, store.ErrConflict) {
		return store.Owner{}, nameTaken(ctx, q, newName, err)
	}
	return r, err
}
