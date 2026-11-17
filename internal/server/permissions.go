// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/marijnkoesen/protodex/gen/buf/registry/iam/v1/iamv1connect"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1/ownerv1connect"
	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/store"
)

// Authorization happens in two layers (see docs/permissions.md):
//
//  1. authzInterceptor applies the per-procedure policy below to every RPC:
//     which procedures may run anonymously, which need a token, which are
//     writes (closed to read-only users without a write grant) and which
//     are reserved for instance admins. Procedures missing from the table
//     are denied.
//  2. Handlers check the caller's permission on each module they touch with
//     modulePermission / requireModule / checkCreate.
//
// Module permission is the highest of:
//
//   - read, for public modules (also for anonymous callers);
//   - admin, for instance admins;
//   - for a user's own namespace and for organization namespaces: admin for
//     members, read for viewers;
//   - the caller's contributor grant on the module (read, write or admin).
//
// Modules of mirror owners are capped at read: they are managed by
// mirroring, never over the API.
//
// A module-scoped token (minted by a CI trust credential, see
// docs/ci-oidc.md) is further capped at its role (read or write) on its
// one module; every other module it sees as an anonymous caller would. It
// may not create modules, manage tokens or call admin procedures. A module the caller cannot read is
// reported as NotFound so that its existence does not leak; a readable
// module the caller may not change is PermissionDenied.

// permission is what a caller may do with a module, in ascending order.
type permission int

const (
	permNone  permission = iota
	permRead             // read metadata, commits, labels, files, graph
	permWrite            // also push and create, move or archive labels
	permAdmin            // also change settings and visibility, delete
)

func (p permission) String() string {
	switch p {
	case permRead:
		return "read"
	case permWrite:
		return "write"
	case permAdmin:
		return "admin"
	}
	return "none"
}

var contributorPermission = map[string]permission{
	store.ContributorRead:  permRead,
	store.ContributorWrite: permWrite,
	store.ContributorAdmin: permAdmin,
}

// roleRank orders instance roles. Unknown or empty roles (organizations
// cannot log in, but be safe) rank as viewer.
func roleRank(role string) int {
	switch role {
	case store.RoleAdmin:
		return 3
	case store.RoleMember:
		return 2
	}
	return 1
}

func isAdmin(id auth.Identity) bool { return id.Owner.Role == store.RoleAdmin }

// isViewer reports whether the caller has the read-only instance role.
func isViewer(id auth.Identity) bool { return roleRank(id.Owner.Role) < roleRank(store.RoleMember) }

// namespacePermission is the permission the caller has on every module of
// o by role alone, without contributor grants or visibility.
func namespacePermission(id auth.Identity, o store.Owner) permission {
	switch {
	case isAdmin(id):
		return permAdmin
	case o.Type == store.OwnerUser && o.ID != id.Owner.ID:
		return permNone // other users' namespaces are private to them
	case isViewer(id):
		return permRead
	}
	return permAdmin
}

// modulePermission returns the caller's permission on module m of owner o.
func modulePermission(ctx context.Context, q store.Querier, o store.Owner, m store.Module) (permission, error) {
	p := permNone
	if m.Visibility == store.VisibilityPublic {
		p = permRead
	}
	id, ok := auth.FromContext(ctx)
	if ok && scoped(id) && id.Token.ModuleID != m.ID {
		ok = false // other modules: no more than anonymous access
	}
	if ok {
		p = max(p, namespacePermission(id, o))
		if p < permAdmin && !o.Mirror {
			g, err := q.GetModuleContributor(ctx, m.ID, id.Owner.ID)
			switch {
			case err == nil:
				p = max(p, contributorPermission[g.Role])
			case !errors.Is(err, store.ErrNotFound):
				return permNone, internalErr(err)
			}
		}
	}
	if ok && scoped(id) {
		// Unknown roles map to permNone: fail closed.
		p = min(p, scopedRolePermission[id.Token.ModuleRole])
	}
	if o.Mirror {
		p = min(p, permRead)
	}
	return p, nil
}

// scoped reports whether the caller's token is restricted to one module.
func scoped(id auth.Identity) bool { return id.Token.ModuleID != "" }

// scopedRolePermission caps module-scoped tokens; admin is never granted.
var scopedRolePermission = map[string]permission{
	store.ContributorRead:  permRead,
	store.ContributorWrite: permWrite,
}

// canRead reports whether the caller may see module m owned by o.
func canRead(ctx context.Context, q store.Querier, o store.Owner, m store.Module) (bool, error) {
	p, err := modulePermission(ctx, q, o, m)
	return p >= permRead, err
}

// requireModule fails unless the caller has at least need on module m of
// owner o: NotFound when the caller cannot even read it, Unauthenticated
// for anonymous callers, PermissionDenied otherwise.
func requireModule(ctx context.Context, q store.Querier, o store.Owner, m store.Module, need permission) error {
	p, err := modulePermission(ctx, q, o, m)
	switch {
	case err != nil:
		return err
	case p >= need:
		return nil
	case p < permRead:
		return moduleNotFound(o.Name, m.Name)
	}
	if _, err := caller(ctx); err != nil {
		return err
	}
	if o.Mirror {
		return errMirror(o)
	}
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s access to module %s/%s required", need, o.Name, m.Name))
}

func errMirror(o store.Owner) error {
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("modules of mirror owner %q are managed by mirroring", o.Name))
}

// checkCreate fails unless the caller may create modules under o.
func checkCreate(ctx context.Context, o store.Owner) error {
	id, err := caller(ctx)
	if err != nil {
		return err
	}
	switch {
	case scoped(id):
		return errScoped
	case o.Mirror:
		return errMirror(o)
	case namespacePermission(id, o) < permAdmin:
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("no permission to create modules under owner %q", o.Name))
	}
	return nil
}

var errScoped = connect.NewError(connect.CodePermissionDenied, errors.New("token is restricted to a single module"))

// scopedAllowed reports whether a module-scoped token may call procedure
// (which needs at least access). Such tokens may only read, write their
// module (with a write role) and identify themselves.
func scopedAllowed(id auth.Identity, procedure string, access procedureAccess) bool {
	switch procedure {
	case ownerv1connect.UserServiceGetCurrentUserProcedure, authnAlphaGetCurrentUser:
		return true
	case modulev1connect.ModuleServiceCreateModulesProcedure:
		return false
	}
	return access == accessWrite && id.Token.ModuleRole == store.ContributorWrite
}

// procedureAccess is the minimum a procedure demands of its caller before
// any handler runs.
type procedureAccess int

const (
	// accessAnonymous: no token needed; handlers apply module visibility.
	accessAnonymous procedureAccess = iota + 1
	// accessAuthenticated: any valid token, whatever the role.
	accessAuthenticated
	// accessWrite: a token of a member or admin, or of a viewer holding at
	// least one write or admin contributor grant.
	accessWrite
	// accessAdmin: a token of an instance admin.
	accessAdmin
)

// procedurePolicy lists every served procedure. Anything missing is denied.
var procedurePolicy = map[string]procedureAccess{
	modulev1connect.CommitServiceGetCommitsProcedure:                      accessAnonymous,
	modulev1connect.CommitServiceListCommitsProcedure:                     accessAnonymous,
	modulev1connect.DownloadServiceDownloadProcedure:                      accessAnonymous,
	modulev1connect.FileDescriptorSetServiceGetFileDescriptorSetProcedure: accessAnonymous,
	modulev1connect.GraphServiceGetGraphProcedure:                         accessAnonymous,
	modulev1connect.LabelServiceGetLabelsProcedure:                        accessAnonymous,
	modulev1connect.LabelServiceListLabelsProcedure:                       accessAnonymous,
	modulev1connect.LabelServiceListLabelHistoryProcedure:                 accessAnonymous,
	modulev1connect.LabelServiceCreateOrUpdateLabelsProcedure:             accessWrite,
	modulev1connect.LabelServiceArchiveLabelsProcedure:                    accessWrite,
	modulev1connect.LabelServiceUnarchiveLabelsProcedure:                  accessWrite,
	modulev1connect.ModuleServiceGetModulesProcedure:                      accessAnonymous,
	modulev1connect.ModuleServiceListModulesProcedure:                     accessAnonymous,
	modulev1connect.ModuleServiceCreateModulesProcedure:                   accessWrite,
	modulev1connect.ModuleServiceUpdateModulesProcedure:                   accessWrite,
	modulev1connect.ModuleServiceDeleteModulesProcedure:                   accessWrite,
	modulev1connect.ResourceServiceGetResourcesProcedure:                  accessAnonymous,
	modulev1connect.UploadServiceUploadProcedure:                          accessWrite,
	ownerv1connect.OwnerServiceGetOwnersProcedure:                         accessAnonymous,
	ownerv1connect.UserServiceGetUsersProcedure:                           accessAnonymous,
	ownerv1connect.UserServiceListUsersProcedure:                          accessAnonymous,
	ownerv1connect.UserServiceGetCurrentUserProcedure:                     accessAuthenticated,
	ownerv1connect.UserServiceCreateUsersProcedure:                        accessAdmin,
	ownerv1connect.UserServiceUpdateUsersProcedure:                        accessAdmin,
	ownerv1connect.UserServiceDeleteUsersProcedure:                        accessAdmin,
	ownerv1connect.OrganizationServiceGetOrganizationsProcedure:           accessAnonymous,
	ownerv1connect.OrganizationServiceListOrganizationsProcedure:          accessAnonymous,
	ownerv1connect.OrganizationServiceCreateOrganizationsProcedure:        accessAdmin,
	ownerv1connect.OrganizationServiceUpdateOrganizationsProcedure:        accessAdmin,
	ownerv1connect.OrganizationServiceDeleteOrganizationsProcedure:        accessAdmin,
	ownerv1connect.OrganizationServiceGetOrganizationSettingsProcedure:    accessAuthenticated,
	ownerv1connect.OrganizationServiceUpdateOrganizationSettingsProcedure: accessAdmin,
	iamv1connect.TokenServiceGetTokensProcedure:                           accessAuthenticated,
	iamv1connect.TokenServiceListTokensProcedure:                          accessAuthenticated,
	iamv1connect.TokenServiceCreateTokensProcedure:                        accessAuthenticated,
	iamv1connect.TokenServiceUpdateTokensProcedure:                        accessAuthenticated,
	iamv1connect.TokenServiceDeleteTokensProcedure:                        accessAuthenticated,
	authnAlphaGetCurrentUser:                                              accessAuthenticated,
}

// authzInterceptor enforces procedurePolicy. It runs after authInterceptor,
// so the caller's identity (if any) is in the context. With a nil store
// nothing is enforced (all services are unimplemented then).
func authzInterceptor(st store.Querier) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if st == nil {
				return next(ctx, req)
			}
			if err := authorizeProcedure(ctx, st, req.Spec().Procedure); err != nil {
				return nil, err
			}
			return next(ctx, req)
		}
	}
}

func authorizeProcedure(ctx context.Context, st store.Querier, procedure string) error {
	access, ok := procedurePolicy[procedure]
	if !ok {
		return connect.NewError(connect.CodePermissionDenied, errors.New("procedure not permitted"))
	}
	if access == accessAnonymous {
		return nil
	}
	id, err := caller(ctx)
	if err != nil {
		return err
	}
	if scoped(id) && !scopedAllowed(id, procedure, access) {
		return errScoped
	}
	switch {
	case access == accessAdmin && !isAdmin(id):
		return connect.NewError(connect.CodePermissionDenied, errors.New("instance admin role required"))
	case access == accessWrite && isViewer(id):
		// Read-only users may only write modules they hold a write grant on.
		grants, err := st.ListUserContributions(ctx, id.Owner.ID)
		if err != nil {
			return internalErr(err)
		}
		for _, g := range grants {
			if contributorPermission[g.Role] >= permWrite {
				return nil
			}
		}
		return connect.NewError(connect.CodePermissionDenied, errors.New("read-only role: write access required"))
	}
	return nil
}

// anonymousAllowed reports whether procedure may be called without
// credentials (subject to module visibility checks in the handlers).
func anonymousAllowed(procedure string) bool {
	return procedurePolicy[procedure] == accessAnonymous
}
