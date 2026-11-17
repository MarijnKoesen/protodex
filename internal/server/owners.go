// SPDX-License-Identifier: Apache-2.0

package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	ownerv1 "github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1/ownerv1connect"
	"github.com/marijnkoesen/protodex/internal/store"
)

// The owner services resolve the owner part of `<host>/<owner>/<module>`
// names. Owners are administered with the `protodex owner` command, so the
// create, update and delete RPCs stay unimplemented.

func userProto(o store.Owner) *ownerv1.User {
	return &ownerv1.User{
		Id:                 o.ID,
		CreateTime:         timestamppb.New(o.CreatedAt),
		UpdateTime:         timestamppb.New(o.UpdatedAt),
		Name:               o.Name,
		Type:               ownerv1.UserType_USER_TYPE_STANDARD,
		State:              ownerv1.UserState_USER_STATE_ACTIVE,
		VerificationStatus: ownerv1.UserVerificationStatus_USER_VERIFICATION_STATUS_UNVERIFIED,
	}
}

func organizationProto(o store.Owner) *ownerv1.Organization {
	return &ownerv1.Organization{
		Id:                 o.ID,
		CreateTime:         timestamppb.New(o.CreatedAt),
		UpdateTime:         timestamppb.New(o.UpdatedAt),
		Name:               o.Name,
		VerificationStatus: ownerv1.OrganizationVerificationStatus_ORGANIZATION_VERIFICATION_STATUS_UNVERIFIED,
		Visibility:         ownerv1.OrganizationVisibility_ORGANIZATION_VISIBILITY_PUBLIC,
	}
}

func ownerProto(o store.Owner) *ownerv1.Owner {
	if o.Type == store.OwnerUser {
		return &ownerv1.Owner{Value: &ownerv1.Owner_User{User: userProto(o)}}
	}
	return &ownerv1.Owner{Value: &ownerv1.Owner_Organization{Organization: organizationProto(o)}}
}

// findOwner looks an owner up by id or name, mapping a miss to NotFound.
// what names the kind of owner in the error ("owner", "user", ...).
func findOwner(ctx context.Context, q store.Querier, id, name, what string) (store.Owner, error) {
	var (
		o   store.Owner
		err error
	)
	switch {
	case id != "":
		o, err = q.GetOwnerByID(ctx, id)
		name = "with id " + id
	case name != "":
		o, err = q.GetOwner(ctx, name)
		name = strconv.Quote(name)
	default:
		return store.Owner{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s reference is empty", what))
	}
	if errors.Is(err, store.ErrNotFound) {
		return store.Owner{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s %s not found", what, name))
	}
	if err != nil {
		return store.Owner{}, internalErr(err)
	}
	return o, nil
}

// resolveOwner resolves an OwnerRef (user or organization) to a stored owner.
// Unknown owners are NotFound. Module services use it for owner names.
func resolveOwner(ctx context.Context, q store.Querier, ref *ownerv1.OwnerRef) (store.Owner, error) {
	return findOwner(ctx, q, ref.GetId(), ref.GetName(), "owner")
}

// resolveUser resolves a UserRef; organizations of that name are NotFound.
func resolveUser(ctx context.Context, q store.Querier, ref *ownerv1.UserRef) (store.Owner, error) {
	o, err := findOwner(ctx, q, ref.GetId(), ref.GetName(), "user")
	if err == nil && o.Type != store.OwnerUser {
		return store.Owner{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("user %q not found", o.Name))
	}
	return o, err
}

// resolveOrganization resolves an OrganizationRef; users are NotFound.
func resolveOrganization(ctx context.Context, q store.Querier, ref *ownerv1.OrganizationRef) (store.Owner, error) {
	o, err := findOwner(ctx, q, ref.GetId(), ref.GetName(), "organization")
	if err == nil && o.Type != store.OwnerOrganization {
		return store.Owner{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("organization %q not found", o.Name))
	}
	return o, err
}

// ownerOrder selects the sort order of a list RPC.
type ownerOrder int

const (
	orderCreateDesc ownerOrder = iota // the API default
	orderCreateAsc
	orderNameDesc
	orderNameAsc
)

func sortOwners(os []store.Owner, order ownerOrder) {
	slices.SortStableFunc(os, func(a, b store.Owner) int {
		var c int
		switch order {
		case orderCreateAsc:
			c = a.CreatedAt.Compare(b.CreatedAt)
		case orderNameDesc:
			c = strings.Compare(b.Name, a.Name)
		case orderNameAsc:
			c = strings.Compare(a.Name, b.Name)
		default:
			c = b.CreatedAt.Compare(a.CreatedAt)
		}
		return cmp.Or(c, strings.Compare(a.ID, b.ID))
	})
}

const maxPageSize = 250

// page returns one page of os. The page token is the decimal offset of the
// page; an empty next token means there are no more results.
func page[T any](os []T, size uint32, token string) ([]T, string, error) {
	off := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 {
			return nil, "", connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
		}
		off = min(n, len(os))
	}
	n := maxPageSize
	if size > 0 {
		n = int(min(size, maxPageSize))
	}
	end := min(off+n, len(os))
	next := ""
	if end < len(os) {
		next = strconv.Itoa(end)
	}
	return os[off:end], next, nil
}

// listOwners returns the owners of type typ whose name contains query,
// ignoring case.
func listOwners(ctx context.Context, q store.Querier, typ, query string) ([]store.Owner, error) {
	query = strings.ToLower(query)
	all, err := q.ListOwners(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	return slices.DeleteFunc(all, func(o store.Owner) bool {
		return o.Type != typ || !strings.Contains(strings.ToLower(o.Name), query)
	}), nil
}

func errMembership() error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("filtering by organization membership is not supported yet"))
}

// ownerService implements buf.registry.owner.v1.OwnerService.
type ownerService struct {
	ownerv1connect.UnimplementedOwnerServiceHandler
	st store.Store
}

// GetOwners resolves owner references; any unknown owner fails the call.
func (s *ownerService) GetOwners(ctx context.Context, req *connect.Request[ownerv1.GetOwnersRequest]) (*connect.Response[ownerv1.GetOwnersResponse], error) {
	resp := &ownerv1.GetOwnersResponse{}
	for _, ref := range req.Msg.GetOwnerRefs() {
		o, err := resolveOwner(ctx, s.st, ref)
		if err != nil {
			return nil, err
		}
		resp.Owners = append(resp.Owners, ownerProto(o))
	}
	return connect.NewResponse(resp), nil
}

// userService implements the read RPCs of buf.registry.owner.v1.UserService.
type userService struct {
	ownerv1connect.UnimplementedUserServiceHandler
	st store.Store
}

// GetUsers resolves user references.
func (s *userService) GetUsers(ctx context.Context, req *connect.Request[ownerv1.GetUsersRequest]) (*connect.Response[ownerv1.GetUsersResponse], error) {
	resp := &ownerv1.GetUsersResponse{}
	for _, ref := range req.Msg.GetUserRefs() {
		o, err := resolveUser(ctx, s.st, ref)
		if err != nil {
			return nil, err
		}
		resp.Users = append(resp.Users, userProto(o))
	}
	return connect.NewResponse(resp), nil
}

// GetCurrentUser returns the authenticated user.
func (s *userService) GetCurrentUser(ctx context.Context, _ *connect.Request[ownerv1.GetCurrentUserRequest]) (*connect.Response[ownerv1.GetCurrentUserResponse], error) {
	me, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownerv1.GetCurrentUserResponse{User: userProto(me.Owner)}), nil
}

// ListUsers lists users. All users are standard and active; instance roles
// and organization membership are not modeled yet, so filtering by them is
// unimplemented.
func (s *userService) ListUsers(ctx context.Context, req *connect.Request[ownerv1.ListUsersRequest]) (*connect.Response[ownerv1.ListUsersResponse], error) {
	m := req.Msg
	if len(m.GetOrganizationRefs()) > 0 {
		return nil, errMembership()
	}
	if len(m.GetHasRoles()) > 0 {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("filtering by instance role is not supported yet"))
	}
	users, err := listOwners(ctx, s.st, store.OwnerUser, m.GetNameQuery())
	if err != nil {
		return nil, err
	}
	if (len(m.GetHasTypes()) > 0 && !slices.Contains(m.GetHasTypes(), ownerv1.UserType_USER_TYPE_STANDARD)) ||
		(len(m.GetHasStates()) > 0 && !slices.Contains(m.GetHasStates(), ownerv1.UserState_USER_STATE_ACTIVE)) {
		users = nil
	}
	order := map[ownerv1.ListUsersRequest_Order]ownerOrder{
		ownerv1.ListUsersRequest_ORDER_CREATE_TIME_ASC: orderCreateAsc,
		ownerv1.ListUsersRequest_ORDER_NAME_DESC:       orderNameDesc,
		ownerv1.ListUsersRequest_ORDER_NAME_ASC:        orderNameAsc,
	}[m.GetOrder()]
	sortOwners(users, order)
	users, next, err := page(users, m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	resp := &ownerv1.ListUsersResponse{NextPageToken: next}
	for _, u := range users {
		resp.Users = append(resp.Users, userProto(u))
	}
	return connect.NewResponse(resp), nil
}

// organizationService implements the read RPCs of
// buf.registry.owner.v1.OrganizationService.
type organizationService struct {
	ownerv1connect.UnimplementedOrganizationServiceHandler
	st store.Store
}

// GetOrganizations resolves organization references (mirror owners included).
func (s *organizationService) GetOrganizations(ctx context.Context, req *connect.Request[ownerv1.GetOrganizationsRequest]) (*connect.Response[ownerv1.GetOrganizationsResponse], error) {
	resp := &ownerv1.GetOrganizationsResponse{}
	for _, ref := range req.Msg.GetOrganizationRefs() {
		o, err := resolveOrganization(ctx, s.st, ref)
		if err != nil {
			return nil, err
		}
		resp.Organizations = append(resp.Organizations, organizationProto(o))
	}
	return connect.NewResponse(resp), nil
}

// ListOrganizations lists organizations, including mirror owners.
func (s *organizationService) ListOrganizations(ctx context.Context, req *connect.Request[ownerv1.ListOrganizationsRequest]) (*connect.Response[ownerv1.ListOrganizationsResponse], error) {
	m := req.Msg
	if len(m.GetUserRefs()) > 0 {
		return nil, errMembership()
	}
	orgs, err := listOwners(ctx, s.st, store.OwnerOrganization, m.GetNameQuery())
	if err != nil {
		return nil, err
	}
	order := map[ownerv1.ListOrganizationsRequest_Order]ownerOrder{
		ownerv1.ListOrganizationsRequest_ORDER_CREATE_TIME_ASC: orderCreateAsc,
		ownerv1.ListOrganizationsRequest_ORDER_NAME_DESC:       orderNameDesc,
		ownerv1.ListOrganizationsRequest_ORDER_NAME_ASC:        orderNameAsc,
	}[m.GetOrder()]
	sortOwners(orgs, order)
	orgs, next, err := page(orgs, m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	resp := &ownerv1.ListOrganizationsResponse{NextPageToken: next}
	for _, o := range orgs {
		resp.Organizations = append(resp.Organizations, organizationProto(o))
	}
	return connect.NewResponse(resp), nil
}
