// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	ownerv1 "github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1"
	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/store"
)

// userToken creates a user with a token and returns the token secret.
func userToken(t *testing.T, st store.Store, name string) (store.Owner, string) {
	t.Helper()
	u := mustOwner(t, st, owner.User, name)
	secret, err := auth.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateToken(context.Background(), store.Token{OwnerID: u.ID, Name: "t", Hash: auth.HashSecret(secret)}); err != nil {
		t.Fatal(err)
	}
	return u, secret
}

func ownerName(n string) *ownerv1.OwnerRef {
	return &ownerv1.OwnerRef{Value: &ownerv1.OwnerRef_Name{Name: n}}
}

func modName(o, m string) *modulev1.ModuleRef {
	return &modulev1.ModuleRef{Value: &modulev1.ModuleRef_Name_{Name: &modulev1.ModuleRef_Name{Owner: o, Module: m}}}
}

func modID(id string) *modulev1.ModuleRef {
	return &modulev1.ModuleRef{Value: &modulev1.ModuleRef_Id{Id: id}}
}

func createModule(t *testing.T, c modulev1connect.ModuleServiceClient, v *modulev1.CreateModulesRequest_Value) *modulev1.Module {
	t.Helper()
	resp, err := c.CreateModules(context.Background(), connect.NewRequest(&modulev1.CreateModulesRequest{
		Values: []*modulev1.CreateModulesRequest_Value{v}}))
	if err != nil {
		t.Fatalf("CreateModules(%v): %v", v, err)
	}
	return resp.Msg.GetModules()[0]
}

func TestModuleServiceCreateGetDelete(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	mustOwner(t, e.st, owner.Organization, "acme")
	mustOwner(t, e.st, owner.Mirror, "googleapis")
	_, bobSecret := userToken(t, e.st, "bob")
	admin := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(e.secret))
	anon := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url)
	bob := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(bobSecret))

	m := createModule(t, admin, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: "weather"})
	if m.GetVisibility() != modulev1.ModuleVisibility_MODULE_VISIBILITY_PRIVATE ||
		m.GetDefaultLabelName() != "main" || m.GetState() != modulev1.ModuleState_MODULE_STATE_ACTIVE ||
		len(m.GetId()) != 32 || m.GetName() != "weather" || m.GetCreateTime() == nil || m.GetUpdateTime() == nil {
		t.Fatalf("created module = %v", m)
	}
	pub := createModule(t, admin, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("admin"), Name: "pub",
		Visibility: modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC, DefaultLabelName: "trunk", Description: "d"})
	if pub.GetDefaultLabelName() != "trunk" || pub.GetDescription() != "d" {
		t.Fatalf("created module = %v", pub)
	}

	// Error codes on create.
	for _, tc := range []struct {
		c    modulev1connect.ModuleServiceClient
		v    *modulev1.CreateModulesRequest_Value
		want connect.Code
	}{
		{anon, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: "x1"}, connect.CodeUnauthenticated},
		{admin, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: "weather"}, connect.CodeAlreadyExists},
		{admin, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("nobody"), Name: "x1"}, connect.CodeNotFound},
		{admin, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: "Bad_Name"}, connect.CodeInvalidArgument},
		{admin, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: "x"}, connect.CodeInvalidArgument},
		{admin, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: "x1", DefaultLabelName: "a b"}, connect.CodeInvalidArgument},
		{admin, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("googleapis"), Name: "x1"}, connect.CodePermissionDenied},
		{bob, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("admin"), Name: "x1"}, connect.CodePermissionDenied},
	} {
		_, err := tc.c.CreateModules(ctx, connect.NewRequest(&modulev1.CreateModulesRequest{
			Values: []*modulev1.CreateModulesRequest_Value{tc.v}}))
		wantCode(t, err, tc.want)
	}
	// A batch is atomic: the valid first value is not created.
	_, err := admin.CreateModules(ctx, connect.NewRequest(&modulev1.CreateModulesRequest{Values: []*modulev1.CreateModulesRequest_Value{
		{OwnerRef: ownerName("acme"), Name: "batch-a"}, {OwnerRef: ownerName("acme"), Name: "batch-a"},
	}}))
	wantCode(t, err, connect.CodeAlreadyExists)
	_, err = admin.GetModules(ctx, connect.NewRequest(&modulev1.GetModulesRequest{ModuleRefs: []*modulev1.ModuleRef{modName("acme", "batch-a")}}))
	wantCode(t, err, connect.CodeNotFound)

	// Get by name and by id; private modules are hidden from anonymous callers.
	got, err := admin.GetModules(ctx, connect.NewRequest(&modulev1.GetModulesRequest{ModuleRefs: []*modulev1.ModuleRef{
		modName("acme", "weather"), modID(pub.GetId()),
	}}))
	if err != nil || len(got.Msg.GetModules()) != 2 || got.Msg.GetModules()[0].GetId() != m.GetId() ||
		got.Msg.GetModules()[1].GetName() != "pub" {
		t.Fatalf("GetModules = %v, %v", got, err)
	}
	if _, err := anon.GetModules(ctx, connect.NewRequest(&modulev1.GetModulesRequest{ModuleRefs: []*modulev1.ModuleRef{modName("admin", "pub")}})); err != nil {
		t.Fatalf("anonymous get of public module: %v", err)
	}
	for _, ref := range []*modulev1.ModuleRef{modName("acme", "weather"), modID(m.GetId()), modName("acme", "nope"), modName("nobody", "x1")} {
		_, err := anon.GetModules(ctx, connect.NewRequest(&modulev1.GetModulesRequest{ModuleRefs: []*modulev1.ModuleRef{ref}}))
		wantCode(t, err, connect.CodeNotFound)
	}
	// Organization modules are visible to any authenticated user.
	if _, err := bob.GetModules(ctx, connect.NewRequest(&modulev1.GetModulesRequest{ModuleRefs: []*modulev1.ModuleRef{modName("acme", "weather")}})); err != nil {
		t.Fatalf("bob get org module: %v", err)
	}

	// Delete: others' namespaces are refused, unknown modules are NotFound.
	_, err = bob.DeleteModules(ctx, connect.NewRequest(&modulev1.DeleteModulesRequest{ModuleRefs: []*modulev1.ModuleRef{modName("admin", "pub")}}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = admin.DeleteModules(ctx, connect.NewRequest(&modulev1.DeleteModulesRequest{ModuleRefs: []*modulev1.ModuleRef{modName("acme", "nope")}}))
	wantCode(t, err, connect.CodeNotFound)
	if _, err := admin.DeleteModules(ctx, connect.NewRequest(&modulev1.DeleteModulesRequest{ModuleRefs: []*modulev1.ModuleRef{modID(m.GetId())}})); err != nil {
		t.Fatal(err)
	}
	_, err = admin.GetModules(ctx, connect.NewRequest(&modulev1.GetModulesRequest{ModuleRefs: []*modulev1.ModuleRef{modName("acme", "weather")}}))
	wantCode(t, err, connect.CodeNotFound)
	// The name is free again.
	createModule(t, admin, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: "weather"})
}

func TestModuleServiceList(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	mustOwner(t, e.st, owner.Organization, "acme")
	admin := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(e.secret))
	anon := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url)
	public := modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC
	for _, v := range []*modulev1.CreateModulesRequest_Value{
		{OwnerRef: ownerName("acme"), Name: "m1", Visibility: public},
		{OwnerRef: ownerName("acme"), Name: "m2"},
		{OwnerRef: ownerName("acme"), Name: "m3", Visibility: public},
		{OwnerRef: ownerName("admin"), Name: "m4", Visibility: public},
	} {
		createModule(t, admin, v)
		time.Sleep(time.Millisecond) // distinct creation times
	}
	names := func(ms []*modulev1.Module) (out []string) {
		for _, m := range ms {
			out = append(out, m.GetName())
		}
		return out
	}
	list := func(c modulev1connect.ModuleServiceClient, req *modulev1.ListModulesRequest) *modulev1.ListModulesResponse {
		t.Helper()
		resp, err := c.ListModules(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("ListModules(%v): %v", req, err)
		}
		return resp.Msg
	}

	// Default order: newest first; paging with an offset token.
	r := list(admin, &modulev1.ListModulesRequest{PageSize: 3})
	if got := names(r.GetModules()); len(got) != 3 || got[0] != "m4" || got[2] != "m2" || r.GetNextPageToken() == "" {
		t.Fatalf("page 1 = %v, next %q", got, r.GetNextPageToken())
	}
	r = list(admin, &modulev1.ListModulesRequest{PageSize: 3, PageToken: r.GetNextPageToken()})
	if got := names(r.GetModules()); len(got) != 1 || got[0] != "m1" || r.GetNextPageToken() != "" {
		t.Fatalf("page 2 = %v, next %q", got, r.GetNextPageToken())
	}
	_, err := admin.ListModules(ctx, connect.NewRequest(&modulev1.ListModulesRequest{PageToken: "x"}))
	wantCode(t, err, connect.CodeInvalidArgument)

	// Owner filter and ascending order.
	r = list(admin, &modulev1.ListModulesRequest{OwnerRefs: []*ownerv1.OwnerRef{ownerName("acme")},
		Order: modulev1.ListModulesRequest_ORDER_CREATE_TIME_ASC})
	if got := names(r.GetModules()); len(got) != 3 || got[0] != "m1" || got[2] != "m3" {
		t.Fatalf("acme modules = %v", got)
	}
	_, err = admin.ListModules(ctx, connect.NewRequest(&modulev1.ListModulesRequest{OwnerRefs: []*ownerv1.OwnerRef{ownerName("nobody")}}))
	wantCode(t, err, connect.CodeNotFound)

	// Anonymous callers only see public modules.
	r = list(anon, &modulev1.ListModulesRequest{OwnerRefs: []*ownerv1.OwnerRef{ownerName("acme")}})
	if got := names(r.GetModules()); len(got) != 2 || got[0] != "m3" || got[1] != "m1" {
		t.Fatalf("anonymous acme modules = %v", got)
	}

	// Update time order follows updates.
	_, err = admin.UpdateModules(ctx, connect.NewRequest(&modulev1.UpdateModulesRequest{Values: []*modulev1.UpdateModulesRequest_Value{
		{ModuleRef: modName("acme", "m1"), Description: new("updated")},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	r = list(admin, &modulev1.ListModulesRequest{Order: modulev1.ListModulesRequest_ORDER_UPDATE_TIME_DESC, PageSize: 1})
	if got := names(r.GetModules()); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("update-time order = %v", got)
	}
}

func TestModuleServiceUpdate(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	mustOwner(t, e.st, owner.Organization, "acme")
	_, bobSecret := userToken(t, e.st, "bob")
	admin := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(e.secret))
	bob := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(bobSecret))
	m := createModule(t, admin, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: "weather"})

	vis := modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC
	state := modulev1.ModuleState_MODULE_STATE_DEPRECATED
	resp, err := admin.UpdateModules(ctx, connect.NewRequest(&modulev1.UpdateModulesRequest{Values: []*modulev1.UpdateModulesRequest_Value{{
		ModuleRef: modName("acme", "weather"), Visibility: &vis, State: &state,
		Description: new("forecasts"), Url: new("https://example.com/weather"), DefaultLabelName: new("dev"),
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	u := resp.Msg.GetModules()[0]
	if u.GetVisibility() != vis || u.GetState() != state || u.GetDescription() != "forecasts" ||
		u.GetUrl() != "https://example.com/weather" || u.GetDefaultLabelName() != "dev" ||
		!u.GetUpdateTime().AsTime().After(m.GetUpdateTime().AsTime()) || !u.GetCreateTime().AsTime().Equal(m.GetCreateTime().AsTime()) {
		t.Fatalf("updated module = %v (was %v)", u, m)
	}
	// Unset fields are left alone.
	resp, err = admin.UpdateModules(ctx, connect.NewRequest(&modulev1.UpdateModulesRequest{Values: []*modulev1.UpdateModulesRequest_Value{{
		ModuleRef: modID(m.GetId()), State: new(modulev1.ModuleState_MODULE_STATE_ACTIVE),
	}}}))
	if err != nil || resp.Msg.GetModules()[0].GetDescription() != "forecasts" ||
		resp.Msg.GetModules()[0].GetState() != modulev1.ModuleState_MODULE_STATE_ACTIVE {
		t.Fatalf("partial update = %v, %v", resp, err)
	}

	// Bob may write organization modules (single-organization model).
	if _, err := bob.UpdateModules(ctx, connect.NewRequest(&modulev1.UpdateModulesRequest{Values: []*modulev1.UpdateModulesRequest_Value{{
		ModuleRef: modName("acme", "weather"), Description: new("by bob"),
	}}})); err != nil {
		t.Fatal(err)
	}
	_, err = admin.UpdateModules(ctx, connect.NewRequest(&modulev1.UpdateModulesRequest{Values: []*modulev1.UpdateModulesRequest_Value{{
		ModuleRef: modName("acme", "nope"), Description: new("x"),
	}}}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = admin.UpdateModules(ctx, connect.NewRequest(&modulev1.UpdateModulesRequest{Values: []*modulev1.UpdateModulesRequest_Value{{
		ModuleRef: modName("acme", "weather"), DefaultLabelName: new("a b"),
	}}}))
	wantCode(t, err, connect.CodeInvalidArgument)
}
