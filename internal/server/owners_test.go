// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownerv1 "github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1/ownerv1connect"
	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/store"
)

func bearer(secret string) connect.ClientOption {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			if secret != "" {
				r.Header().Set("Authorization", "Bearer "+secret)
			}
			return next(ctx, r)
		}
	}))
}

func mustOwner(t *testing.T, st store.Store, kind owner.Kind, name string) store.Owner {
	t.Helper()
	o, err := owner.Create(context.Background(), st, kind, name)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestOwnerServiceResolvesOwners(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	org := mustOwner(t, e.st, owner.Organization, "acme")
	mirror := mustOwner(t, e.st, owner.Mirror, "googleapis")
	admin, err := e.st.GetOwner(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Owner lookups are reads: they work anonymously and with a token.
	for _, secret := range []string{"", e.secret} {
		c := ownerv1connect.NewOwnerServiceClient(http.DefaultClient, e.url, bearer(secret))
		resp, err := c.GetOwners(ctx, connect.NewRequest(&ownerv1.GetOwnersRequest{OwnerRefs: []*ownerv1.OwnerRef{
			{Value: &ownerv1.OwnerRef_Name{Name: "acme"}},
			{Value: &ownerv1.OwnerRef_Name{Name: "admin"}},
			{Value: &ownerv1.OwnerRef_Id{Id: mirror.ID}},
		}}))
		if err != nil {
			t.Fatalf("GetOwners: %v", err)
		}
		got := resp.Msg.GetOwners()
		if len(got) != 3 ||
			got[0].GetOrganization().GetId() != org.ID || got[0].GetOrganization().GetName() != "acme" ||
			got[1].GetUser().GetId() != admin.ID || got[1].GetUser().GetName() != "admin" ||
			got[2].GetOrganization().GetName() != "googleapis" {
			t.Fatalf("GetOwners = %v", got)
		}
		if ct := got[0].GetOrganization().GetCreateTime().AsTime(); !ct.Equal(org.CreatedAt) {
			t.Fatalf("create_time = %v, want %v", ct, org.CreatedAt)
		}

		// Unknown owners are NotFound, by name and by id.
		for _, ref := range []*ownerv1.OwnerRef{
			{Value: &ownerv1.OwnerRef_Name{Name: "nobody"}},
			{Value: &ownerv1.OwnerRef_Id{Id: "0123456789abcdef0123456789abcdef"}},
		} {
			_, err := c.GetOwners(ctx, connect.NewRequest(&ownerv1.GetOwnersRequest{OwnerRefs: []*ownerv1.OwnerRef{
				{Value: &ownerv1.OwnerRef_Name{Name: "acme"}}, ref,
			}}))
			wantCode(t, err, connect.CodeNotFound)
		}
	}

	// A renamed owner resolves under its new name, with the same id.
	if _, err := owner.Rename(ctx, e.st, "acme", "acme-corp"); err != nil {
		t.Fatal(err)
	}
	c := ownerv1connect.NewOwnerServiceClient(http.DefaultClient, e.url)
	resp, err := c.GetOwners(ctx, connect.NewRequest(&ownerv1.GetOwnersRequest{OwnerRefs: []*ownerv1.OwnerRef{
		{Value: &ownerv1.OwnerRef_Name{Name: "acme-corp"}},
	}}))
	if err != nil || resp.Msg.GetOwners()[0].GetOrganization().GetId() != org.ID {
		t.Fatalf("renamed owner: %v %v", resp, err)
	}
	_, err = c.GetOwners(ctx, connect.NewRequest(&ownerv1.GetOwnersRequest{OwnerRefs: []*ownerv1.OwnerRef{
		{Value: &ownerv1.OwnerRef_Name{Name: "acme"}},
	}}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestUserService(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	mustOwner(t, e.st, owner.Organization, "acme")
	time.Sleep(time.Millisecond) // distinct creation times for ordering
	bob := mustOwner(t, e.st, owner.User, "bob")
	c := ownerv1connect.NewUserServiceClient(http.DefaultClient, e.url, bearer(e.secret))

	cur, err := c.GetCurrentUser(ctx, connect.NewRequest(&ownerv1.GetCurrentUserRequest{}))
	if err != nil || cur.Msg.GetUser().GetName() != "admin" ||
		cur.Msg.GetUser().GetState() != ownerv1.UserState_USER_STATE_ACTIVE ||
		cur.Msg.GetUser().GetType() != ownerv1.UserType_USER_TYPE_STANDARD {
		t.Fatalf("GetCurrentUser = %v, %v", cur, err)
	}
	_, err = ownerv1connect.NewUserServiceClient(http.DefaultClient, e.url).
		GetCurrentUser(ctx, connect.NewRequest(&ownerv1.GetCurrentUserRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)

	got, err := c.GetUsers(ctx, connect.NewRequest(&ownerv1.GetUsersRequest{UserRefs: []*ownerv1.UserRef{
		{Value: &ownerv1.UserRef_Name{Name: "bob"}},
		{Value: &ownerv1.UserRef_Id{Id: bob.ID}},
	}}))
	if err != nil || len(got.Msg.GetUsers()) != 2 || got.Msg.GetUsers()[1].GetName() != "bob" {
		t.Fatalf("GetUsers = %v, %v", got, err)
	}
	// An organization is not a user.
	_, err = c.GetUsers(ctx, connect.NewRequest(&ownerv1.GetUsersRequest{UserRefs: []*ownerv1.UserRef{
		{Value: &ownerv1.UserRef_Name{Name: "acme"}},
	}}))
	wantCode(t, err, connect.CodeNotFound)

	list := func(req *ownerv1.ListUsersRequest) ([]string, string) {
		t.Helper()
		resp, err := c.ListUsers(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("ListUsers(%v): %v", req, err)
		}
		var names []string
		for _, u := range resp.Msg.GetUsers() {
			names = append(names, u.GetName())
		}
		return names, resp.Msg.GetNextPageToken()
	}
	if names, _ := list(&ownerv1.ListUsersRequest{}); len(names) != 2 || names[0] != "bob" {
		t.Fatalf("default order (newest first) = %v", names)
	}
	if names, _ := list(&ownerv1.ListUsersRequest{Order: ownerv1.ListUsersRequest_ORDER_NAME_ASC}); len(names) != 2 || names[0] != "admin" {
		t.Fatalf("name order = %v", names)
	}
	if names, _ := list(&ownerv1.ListUsersRequest{NameQuery: "ob"}); len(names) != 1 || names[0] != "bob" {
		t.Fatalf("name query = %v", names)
	}
	if names, _ := list(&ownerv1.ListUsersRequest{NameQuery: "oB"}); len(names) != 1 || names[0] != "bob" {
		t.Fatalf("mixed-case query: %v", names)
	}
	if names, _ := list(&ownerv1.ListUsersRequest{HasTypes: []ownerv1.UserType{ownerv1.UserType_USER_TYPE_BOT}}); len(names) != 0 {
		t.Fatalf("bot filter = %v", names)
	}
	names, next := list(&ownerv1.ListUsersRequest{PageSize: 1, Order: ownerv1.ListUsersRequest_ORDER_NAME_ASC})
	if len(names) != 1 || names[0] != "admin" || next == "" {
		t.Fatalf("page 1 = %v %q", names, next)
	}
	names, next = list(&ownerv1.ListUsersRequest{PageSize: 1, PageToken: next, Order: ownerv1.ListUsersRequest_ORDER_NAME_ASC})
	if len(names) != 1 || names[0] != "bob" || next != "" {
		t.Fatalf("page 2 = %v %q", names, next)
	}
	_, err = c.ListUsers(ctx, connect.NewRequest(&ownerv1.ListUsersRequest{PageToken: "x"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = c.ListUsers(ctx, connect.NewRequest(&ownerv1.ListUsersRequest{OrganizationRefs: []*ownerv1.OrganizationRef{
		{Value: &ownerv1.OrganizationRef_Name{Name: "acme"}},
	}}))
	wantCode(t, err, connect.CodeUnimplemented)

}

func TestUserAdministration(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	org := mustOwner(t, e.st, owner.Organization, "acme")
	c := ownerv1connect.NewUserServiceClient(http.DefaultClient, e.url, bearer(e.secret))
	ref := func(name string) []*ownerv1.UserRef {
		return []*ownerv1.UserRef{{Value: &ownerv1.UserRef_Name{Name: name}}}
	}

	created, err := c.CreateUsers(ctx, connect.NewRequest(&ownerv1.CreateUsersRequest{Values: []*ownerv1.CreateUsersRequest_Value{
		{Name: "eve", Description: "hi", Url: "https://example.com"}}}))
	if err != nil || created.Msg.GetUsers()[0].GetName() != "eve" || created.Msg.GetUsers()[0].GetDescription() != "hi" {
		t.Fatalf("CreateUsers = %v, %v", created, err)
	}
	_, err = c.CreateUsers(ctx, connect.NewRequest(&ownerv1.CreateUsersRequest{Values: []*ownerv1.CreateUsersRequest_Value{{Name: "eve"}}}))
	wantCode(t, err, connect.CodeAlreadyExists)
	_, err = c.CreateUsers(ctx, connect.NewRequest(&ownerv1.CreateUsersRequest{Values: []*ownerv1.CreateUsersRequest_Value{{Name: "acme"}}}))
	wantCode(t, err, connect.CodeAlreadyExists)
	_, err = c.CreateUsers(ctx, connect.NewRequest(&ownerv1.CreateUsersRequest{Values: []*ownerv1.CreateUsersRequest_Value{{Name: "Bad_Name"}}}))
	wantCode(t, err, connect.CodeInvalidArgument)
	// All or nothing.
	_, err = c.CreateUsers(ctx, connect.NewRequest(&ownerv1.CreateUsersRequest{Values: []*ownerv1.CreateUsersRequest_Value{{Name: "zed"}, {Name: "eve"}}}))
	wantCode(t, err, connect.CodeAlreadyExists)
	if _, err := e.st.GetOwner(ctx, "zed"); err == nil {
		t.Fatal("zed created despite failed batch")
	}

	desc := "new"
	up, err := c.UpdateUsers(ctx, connect.NewRequest(&ownerv1.UpdateUsersRequest{Values: []*ownerv1.UpdateUsersRequest_Value{
		{UserRef: ref("eve")[0], Description: &desc}}}))
	if err != nil || up.Msg.GetUsers()[0].GetDescription() != "new" || up.Msg.GetUsers()[0].GetUrl() != "https://example.com" {
		t.Fatalf("UpdateUsers = %v, %v", up, err)
	}
	inactive := ownerv1.UserState_USER_STATE_INACTIVE
	_, err = c.UpdateUsers(ctx, connect.NewRequest(&ownerv1.UpdateUsersRequest{Values: []*ownerv1.UpdateUsersRequest_Value{
		{UserRef: ref("eve")[0], State: &inactive}}}))
	wantCode(t, err, connect.CodeUnimplemented)

	// Deleting: not yourself, not a user who owns modules, not an org.
	_, err = c.DeleteUsers(ctx, connect.NewRequest(&ownerv1.DeleteUsersRequest{UserRefs: ref("admin")}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = c.DeleteUsers(ctx, connect.NewRequest(&ownerv1.DeleteUsersRequest{UserRefs: ref("acme")}))
	wantCode(t, err, connect.CodeNotFound)
	eve, _ := e.st.GetOwner(ctx, "eve")
	if _, err := e.st.CreateModule(ctx, store.Module{OwnerID: eve.ID, Name: "m", Visibility: store.VisibilityPrivate, DefaultLabel: "main"}); err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteUsers(ctx, connect.NewRequest(&ownerv1.DeleteUsersRequest{UserRefs: ref("eve")}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	m, _ := e.st.GetModule(ctx, eve.ID, "m")
	if err := e.st.DeleteModule(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteUsers(ctx, connect.NewRequest(&ownerv1.DeleteUsersRequest{UserRefs: ref("eve")})); err != nil {
		t.Fatalf("DeleteUsers: %v", err)
	}
	if _, err := e.st.GetOwner(ctx, "eve"); err == nil {
		t.Fatal("eve still exists")
	}
	if _, err := e.st.GetOwnerByID(ctx, org.ID); err != nil {
		t.Fatal(err)
	}
}

func TestOrganizationService(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	org := mustOwner(t, e.st, owner.Organization, "acme")
	mustOwner(t, e.st, owner.Mirror, "googleapis")
	c := ownerv1connect.NewOrganizationServiceClient(http.DefaultClient, e.url)

	got, err := c.GetOrganizations(ctx, connect.NewRequest(&ownerv1.GetOrganizationsRequest{OrganizationRefs: []*ownerv1.OrganizationRef{
		{Value: &ownerv1.OrganizationRef_Id{Id: org.ID}},
		{Value: &ownerv1.OrganizationRef_Name{Name: "googleapis"}},
	}}))
	if err != nil || len(got.Msg.GetOrganizations()) != 2 || got.Msg.GetOrganizations()[0].GetName() != "acme" ||
		got.Msg.GetOrganizations()[0].GetVisibility() != ownerv1.OrganizationVisibility_ORGANIZATION_VISIBILITY_PUBLIC {
		t.Fatalf("GetOrganizations = %v, %v", got, err)
	}
	for _, name := range []string{"admin", "nobody"} { // a user is not an organization
		_, err = c.GetOrganizations(ctx, connect.NewRequest(&ownerv1.GetOrganizationsRequest{OrganizationRefs: []*ownerv1.OrganizationRef{
			{Value: &ownerv1.OrganizationRef_Name{Name: name}},
		}}))
		wantCode(t, err, connect.CodeNotFound)
	}

	list, err := c.ListOrganizations(ctx, connect.NewRequest(&ownerv1.ListOrganizationsRequest{Order: ownerv1.ListOrganizationsRequest_ORDER_NAME_DESC}))
	if err != nil || len(list.Msg.GetOrganizations()) != 2 || list.Msg.GetOrganizations()[0].GetName() != "googleapis" {
		t.Fatalf("ListOrganizations = %v, %v", list, err)
	}
	list, err = c.ListOrganizations(ctx, connect.NewRequest(&ownerv1.ListOrganizationsRequest{NameQuery: "ACME"}))
	if err != nil || len(list.Msg.GetOrganizations()) != 1 || list.Msg.GetOrganizations()[0].GetName() != "acme" {
		t.Fatalf("ListOrganizations(ACME) = %v, %v", list, err)
	}
	_, err = c.ListOrganizations(ctx, connect.NewRequest(&ownerv1.ListOrganizationsRequest{UserRefs: []*ownerv1.UserRef{
		{Value: &ownerv1.UserRef_Name{Name: "admin"}},
	}}))
	wantCode(t, err, connect.CodeUnimplemented)

	// Writes need an admin token.
	_, err = c.CreateOrganizations(ctx, connect.NewRequest(&ownerv1.CreateOrganizationsRequest{Values: []*ownerv1.CreateOrganizationsRequest_Value{{Name: "x"}}}))
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestOrganizationAdministration(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	mustOwner(t, e.st, owner.Mirror, "googleapis")
	c := ownerv1connect.NewOrganizationServiceClient(http.DefaultClient, e.url, bearer(e.secret))
	ref := func(name string) []*ownerv1.OrganizationRef {
		return []*ownerv1.OrganizationRef{{Value: &ownerv1.OrganizationRef_Name{Name: name}}}
	}

	created, err := c.CreateOrganizations(ctx, connect.NewRequest(&ownerv1.CreateOrganizationsRequest{Values: []*ownerv1.CreateOrganizationsRequest_Value{
		{Name: "acme", Description: "Acme Inc", Url: "https://acme.example"}}}))
	if err != nil || created.Msg.GetOrganizations()[0].GetDescription() != "Acme Inc" {
		t.Fatalf("CreateOrganizations = %v, %v", created, err)
	}
	for _, name := range []string{"acme", "admin", "googleapis"} {
		_, err = c.CreateOrganizations(ctx, connect.NewRequest(&ownerv1.CreateOrganizationsRequest{Values: []*ownerv1.CreateOrganizationsRequest_Value{{Name: name}}}))
		wantCode(t, err, connect.CodeAlreadyExists)
	}
	private := ownerv1.OrganizationVisibility_ORGANIZATION_VISIBILITY_PRIVATE
	_, err = c.CreateOrganizations(ctx, connect.NewRequest(&ownerv1.CreateOrganizationsRequest{Values: []*ownerv1.CreateOrganizationsRequest_Value{{Name: "p", Visibility: &private}}}))
	wantCode(t, err, connect.CodeUnimplemented)

	url := "https://new.example"
	up, err := c.UpdateOrganizations(ctx, connect.NewRequest(&ownerv1.UpdateOrganizationsRequest{Values: []*ownerv1.UpdateOrganizationsRequest_Value{
		{OrganizationRef: ref("acme")[0], Url: &url}}}))
	if err != nil || up.Msg.GetOrganizations()[0].GetUrl() != url || up.Msg.GetOrganizations()[0].GetDescription() != "Acme Inc" {
		t.Fatalf("UpdateOrganizations = %v, %v", up, err)
	}
	_, err = c.UpdateOrganizations(ctx, connect.NewRequest(&ownerv1.UpdateOrganizationsRequest{Values: []*ownerv1.UpdateOrganizationsRequest_Value{
		{OrganizationRef: ref("googleapis")[0], Url: &url}}}))
	wantCode(t, err, connect.CodeFailedPrecondition)

	// Mirrors and organizations with modules cannot be deleted.
	_, err = c.DeleteOrganizations(ctx, connect.NewRequest(&ownerv1.DeleteOrganizationsRequest{OrganizationRefs: ref("googleapis")}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	acme, _ := e.st.GetOwner(ctx, "acme")
	mod, err := e.st.CreateModule(ctx, store.Module{OwnerID: acme.ID, Name: "m", Visibility: store.VisibilityPrivate, DefaultLabel: "main"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteOrganizations(ctx, connect.NewRequest(&ownerv1.DeleteOrganizationsRequest{OrganizationRefs: ref("acme")}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = c.DeleteOrganizations(ctx, connect.NewRequest(&ownerv1.DeleteOrganizationsRequest{OrganizationRefs: ref("admin")}))
	wantCode(t, err, connect.CodeNotFound)
	if err := e.st.DeleteModule(ctx, mod.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteOrganizations(ctx, connect.NewRequest(&ownerv1.DeleteOrganizationsRequest{OrganizationRefs: ref("acme")})); err != nil {
		t.Fatalf("DeleteOrganizations: %v", err)
	}
	_, err = c.DeleteOrganizations(ctx, connect.NewRequest(&ownerv1.DeleteOrganizationsRequest{OrganizationRefs: ref("acme")}))
	wantCode(t, err, connect.CodeNotFound)
}
