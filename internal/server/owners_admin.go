// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	ownerv1 "github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1"
	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/store"
)

// Owner administration over the API (`buf registry organization|user ...`).
// The procedure policy already limits these RPCs to instance admins. Only
// what the store models is supported: names and, for users, the active
// state, description and URL. Verification and visibility are not modeled, so
// setting them to anything but the defaults is Unimplemented rather than
// silently dropped.

func errUnsupportedField(what string) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%s is not supported: protodex does not store it", what))
}

// ownerAdminErr maps an owner package error to a Connect error.
func ownerAdminErr(err error) error {
	switch {
	case errors.Is(err, owner.ErrInvalidName):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, store.ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, errors.New(strings.TrimPrefix(err.Error(), "store: conflict: ")))
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	}
	return internalErr(err)
}

func createOwner(ctx context.Context, q store.Querier, kind owner.Kind, name, description, url string) (store.Owner, error) {
	o, err := owner.Create(ctx, q, kind, name)
	if err != nil {
		return store.Owner{}, ownerAdminErr(err)
	}
	if description == "" && url == "" {
		return o, nil
	}
	o, err = q.SetOwnerProfile(ctx, o.ID, description, url)
	if err != nil {
		return store.Owner{}, ownerAdminErr(err)
	}
	return o, nil
}

// updateProfile applies the optional description and url to o.
func updateProfile(ctx context.Context, q store.Querier, o store.Owner, description, url *string) (store.Owner, error) {
	if description == nil && url == nil {
		return o, nil
	}
	d, u := o.Description, o.URL
	if description != nil {
		d = *description
	}
	if url != nil {
		u = *url
	}
	o, err := q.SetOwnerProfile(ctx, o.ID, d, u)
	if err != nil {
		return store.Owner{}, ownerAdminErr(err)
	}
	return o, nil
}

// deleteOwner deletes o unless it is a mirror owner, the caller, or still
// owns modules.
func deleteOwner(ctx context.Context, q store.Querier, o store.Owner, self string) error {
	switch {
	case o.Mirror:
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("mirror owner %q is managed by mirroring and cannot be deleted", o.Name))
	case o.ID == self:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("you cannot delete yourself"))
	}
	mods, err := q.ListModules(ctx, o.ID)
	if err != nil {
		return internalErr(err)
	}
	if len(mods) > 0 {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%q still owns %d module(s): delete them first", o.Name, len(mods)))
	}
	if err := q.DeleteOwner(ctx, o.ID); err != nil {
		return ownerAdminErr(err)
	}
	return nil
}

// CreateOrganizations creates organizations atomically.
func (s *organizationService) CreateOrganizations(ctx context.Context, req *connect.Request[ownerv1.CreateOrganizationsRequest]) (*connect.Response[ownerv1.CreateOrganizationsResponse], error) {
	resp := &ownerv1.CreateOrganizationsResponse{}
	err := s.st.WithTx(ctx, func(q store.Querier) error {
		for _, v := range req.Msg.GetValues() {
			if v.Visibility != nil && v.GetVisibility() != ownerv1.OrganizationVisibility_ORGANIZATION_VISIBILITY_PUBLIC {
				return errUnsupportedField("organization visibility other than public")
			}
			o, err := createOwner(ctx, q, owner.Organization, v.GetName(), v.GetDescription(), v.GetUrl())
			if err != nil {
				return err
			}
			resp.Organizations = append(resp.Organizations, organizationProto(o))
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(resp), nil
}

// UpdateOrganizations changes description and URL, atomically.
func (s *organizationService) UpdateOrganizations(ctx context.Context, req *connect.Request[ownerv1.UpdateOrganizationsRequest]) (*connect.Response[ownerv1.UpdateOrganizationsResponse], error) {
	resp := &ownerv1.UpdateOrganizationsResponse{}
	err := s.st.WithTx(ctx, func(q store.Querier) error {
		for _, v := range req.Msg.GetValues() {
			o, err := resolveOrganization(ctx, q, v.GetOrganizationRef())
			if err != nil {
				return err
			}
			if o.Mirror {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("mirror owner %q is managed by mirroring", o.Name))
			}
			if v.VerificationStatus != nil ||
				(v.Visibility != nil && v.GetVisibility() != ownerv1.OrganizationVisibility_ORGANIZATION_VISIBILITY_PUBLIC) {
				return errUnsupportedField("organization verification and visibility other than public")
			}
			o, err = updateProfile(ctx, q, o, v.Description, v.Url)
			if err != nil {
				return err
			}
			resp.Organizations = append(resp.Organizations, organizationProto(o))
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(resp), nil
}

// DeleteOrganizations deletes organizations that own no modules, atomically.
func (s *organizationService) DeleteOrganizations(ctx context.Context, req *connect.Request[ownerv1.DeleteOrganizationsRequest]) (*connect.Response[ownerv1.DeleteOrganizationsResponse], error) {
	me, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	err = s.st.WithTx(ctx, func(q store.Querier) error {
		for _, ref := range req.Msg.GetOrganizationRefs() {
			o, err := resolveOrganization(ctx, q, ref)
			if err != nil {
				return err
			}
			if err := deleteOwner(ctx, q, o, me.Owner.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&ownerv1.DeleteOrganizationsResponse{}), nil
}

// CreateUsers creates standard users with the default (member) role.
// Credentials are issued separately (TokenService or `protodex owner`).
func (s *userService) CreateUsers(ctx context.Context, req *connect.Request[ownerv1.CreateUsersRequest]) (*connect.Response[ownerv1.CreateUsersResponse], error) {
	resp := &ownerv1.CreateUsersResponse{}
	err := s.st.WithTx(ctx, func(q store.Querier) error {
		for _, v := range req.Msg.GetValues() {
			if v.GetType() > ownerv1.UserType_USER_TYPE_STANDARD {
				return errUnsupportedField("user types other than standard")
			}
			if v.GetVerificationStatus() > ownerv1.UserVerificationStatus_USER_VERIFICATION_STATUS_UNVERIFIED {
				return errUnsupportedField("user verification")
			}
			o, err := createOwner(ctx, q, owner.User, v.GetName(), v.GetDescription(), v.GetUrl())
			if err != nil {
				return err
			}
			resp.Users = append(resp.Users, userProto(o))
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(resp), nil
}

// UpdateUsers changes description and URL, atomically. Users are always
// active, so any other state is Unimplemented.
func (s *userService) UpdateUsers(ctx context.Context, req *connect.Request[ownerv1.UpdateUsersRequest]) (*connect.Response[ownerv1.UpdateUsersResponse], error) {
	resp := &ownerv1.UpdateUsersResponse{}
	err := s.st.WithTx(ctx, func(q store.Querier) error {
		for _, v := range req.Msg.GetValues() {
			o, err := resolveUser(ctx, q, v.GetUserRef())
			if err != nil {
				return err
			}
			if (v.State != nil && v.GetState() != ownerv1.UserState_USER_STATE_ACTIVE) ||
				(v.VerificationStatus != nil && v.GetVerificationStatus() != ownerv1.UserVerificationStatus_USER_VERIFICATION_STATUS_UNVERIFIED) {
				return errUnsupportedField("user state other than active and verification")
			}
			o, err = updateProfile(ctx, q, o, v.Description, v.Url)
			if err != nil {
				return err
			}
			resp.Users = append(resp.Users, userProto(o))
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(resp), nil
}

// DeleteUsers deletes users that own no modules, with their tokens, atomically.
func (s *userService) DeleteUsers(ctx context.Context, req *connect.Request[ownerv1.DeleteUsersRequest]) (*connect.Response[ownerv1.DeleteUsersResponse], error) {
	me, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	err = s.st.WithTx(ctx, func(q store.Querier) error {
		for _, ref := range req.Msg.GetUserRefs() {
			o, err := resolveUser(ctx, q, ref)
			if err != nil {
				return err
			}
			if err := deleteOwner(ctx, q, o, me.Owner.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&ownerv1.DeleteUsersResponse{}), nil
}
