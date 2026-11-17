// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	ownerv1 "github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1/ownerv1connect"
	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/store"
)

// userWithRole creates a user with the given instance role and returns it
// with a token secret.
func userWithRole(t *testing.T, st store.Store, name, role string) (store.Owner, string) {
	t.Helper()
	u, secret := userToken(t, st, name)
	if err := st.SetOwnerRole(context.Background(), u.ID, role); err != nil {
		t.Fatal(err)
	}
	return u, secret
}

func grant(t *testing.T, st store.Store, o, m string, u store.Owner, role string) {
	t.Helper()
	ctx := context.Background()
	ow, err := st.GetOwner(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := st.GetModule(ctx, ow.ID, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetModuleContributor(ctx, store.ModuleContributor{ModuleID: mod.ID, UserID: u.ID, Role: role}); err != nil {
		t.Fatal(err)
	}
}

// mirrorModule creates a module of a mirror owner directly in the store
// (the API refuses to).
func mirrorModule(t *testing.T, st store.Store, o store.Owner, name, visibility string) {
	t.Helper()
	if _, err := st.CreateModule(context.Background(), store.Module{OwnerID: o.ID, Name: name, Visibility: visibility, DefaultLabel: "main"}); err != nil {
		t.Fatal(err)
	}
}

type caller struct {
	mods modulev1connect.ModuleServiceClient
	up   modulev1connect.UploadServiceClient
}

func newCaller(url, secret string) caller {
	var opts []connect.ClientOption
	if secret != "" {
		opts = append(opts, bearer(secret))
	}
	return caller{
		mods: modulev1connect.NewModuleServiceClient(http.DefaultClient, url, opts...),
		up:   modulev1connect.NewUploadServiceClient(http.DefaultClient, url, opts...),
	}
}

func codeOf(err error) connect.Code {
	if err == nil {
		return 0
	}
	return connect.CodeOf(err)
}

// TestAuthzMatrix checks role x action x visibility against a hand-written
// expectation table (not derived from the implementation).
func TestAuthzMatrix(t *testing.T) {
	e := newAuthEnv(t) // "admin" is the bootstrapped instance admin
	ctx := context.Background()
	mustOwner(t, e.st, owner.Organization, "acme")
	mirror := mustOwner(t, e.st, owner.Mirror, "googleapis")
	_, aliceSecret := userWithRole(t, e.st, "alice", store.RoleMember)
	bob, bobSecret := userWithRole(t, e.st, "bob", store.RoleMember)
	_, veraSecret := userWithRole(t, e.st, "vera", store.RoleViewer)
	carl, carlSecret := userWithRole(t, e.st, "carl", store.RoleViewer)

	adminC := newCaller(e.url, e.secret)
	for _, m := range []struct {
		owner, name string
		public      bool
	}{{"acme", "pub", true}, {"acme", "priv", false}, {"alice", "apub", true}, {"alice", "apriv", false}} {
		v := modulev1.ModuleVisibility_MODULE_VISIBILITY_PRIVATE
		if m.public {
			v = modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC
		}
		createModule(t, adminC.mods, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName(m.owner), Name: m.name, Visibility: v})
	}
	mirrorModule(t, e.st, mirror, "gpub", store.VisibilityPublic)
	mirrorModule(t, e.st, mirror, "gpriv", store.VisibilityPrivate)
	grant(t, e.st, "alice", "apriv", bob, store.ContributorRead)
	grant(t, e.st, "acme", "priv", carl, store.ContributorWrite)

	const (
		ok     = connect.Code(0)
		nf     = connect.CodeNotFound
		pd     = connect.CodePermissionDenied
		unauth = connect.CodeUnauthenticated
	)
	modules := []string{"acme/pub", "acme/priv", "alice/apub", "alice/apriv", "googleapis/gpub", "googleapis/gpriv"}
	// For each caller and action: the expected code per module, in the
	// order of modules.
	type row struct{ read, write, admin [6]connect.Code }
	want := map[string]row{
		"anonymous": {
			read:  [6]connect.Code{ok, nf, ok, nf, ok, nf},
			write: [6]connect.Code{unauth, unauth, unauth, unauth, unauth, unauth},
			admin: [6]connect.Code{unauth, unauth, unauth, unauth, unauth, unauth},
		},
		"viewer": {
			read:  [6]connect.Code{ok, ok, ok, nf, ok, ok},
			write: [6]connect.Code{pd, pd, pd, pd, pd, pd}, // rejected by the interceptor
			admin: [6]connect.Code{pd, pd, pd, pd, pd, pd},
		},
		"viewer+write grant on acme/priv": {
			read:  [6]connect.Code{ok, ok, ok, nf, ok, ok},
			write: [6]connect.Code{pd, ok, pd, nf, pd, pd},
			admin: [6]connect.Code{pd, pd, pd, nf, pd, pd},
		},
		"member+read grant on alice/apriv": {
			read:  [6]connect.Code{ok, ok, ok, ok, ok, ok},
			write: [6]connect.Code{ok, ok, pd, pd, pd, pd},
			admin: [6]connect.Code{ok, ok, pd, pd, pd, pd},
		},
		"member owning alice/*": {
			read:  [6]connect.Code{ok, ok, ok, ok, ok, ok},
			write: [6]connect.Code{ok, ok, ok, ok, pd, pd},
			admin: [6]connect.Code{ok, ok, ok, ok, pd, pd},
		},
		"admin": {
			read:  [6]connect.Code{ok, ok, ok, ok, ok, ok},
			write: [6]connect.Code{ok, ok, ok, ok, pd, pd},
			admin: [6]connect.Code{ok, ok, ok, ok, pd, pd},
		},
	}
	callers := map[string]caller{
		"anonymous":                        newCaller(e.url, ""),
		"viewer":                           newCaller(e.url, veraSecret),
		"viewer+write grant on acme/priv":  newCaller(e.url, carlSecret),
		"member+read grant on alice/apriv": newCaller(e.url, bobSecret),
		"member owning alice/*":            newCaller(e.url, aliceSecret),
		"admin":                            adminC,
	}
	for name, c := range callers {
		for i, full := range modules {
			o, m, _ := strings.Cut(full, "/")
			ref := modName(o, m)
			_, err := c.mods.GetModules(ctx, connect.NewRequest(&modulev1.GetModulesRequest{ModuleRefs: []*modulev1.ModuleRef{ref}}))
			check(t, name, "read", full, err, want[name].read[i])

			_, err = c.up.Upload(ctx, connect.NewRequest(&modulev1.UploadRequest{Contents: []*modulev1.UploadRequest_Content{
				{ModuleRef: ref, Files: protoFiles("a.proto", fmt.Sprintf("// %s %s", name, full))}}}))
			check(t, name, "push", full, err, want[name].write[i])

			desc := "by " + name
			_, err = c.mods.UpdateModules(ctx, connect.NewRequest(&modulev1.UpdateModulesRequest{Values: []*modulev1.UpdateModulesRequest_Value{
				{ModuleRef: ref, Description: &desc}}}))
			check(t, name, "update settings", full, err, want[name].admin[i])
		}
	}

	// Creating modules under an owner.
	for _, tc := range []struct {
		caller, owner string
		want          connect.Code
	}{
		{"anonymous", "acme", unauth},
		{"viewer", "acme", pd},
		{"viewer+write grant on acme/priv", "acme", pd},
		{"viewer+write grant on acme/priv", "carl", pd}, // read-only even in their own namespace
		{"member+read grant on alice/apriv", "acme", ok},
		{"member+read grant on alice/apriv", "bob", ok},
		{"member+read grant on alice/apriv", "alice", pd},
		{"member owning alice/*", "googleapis", pd},
		{"admin", "alice", ok},
		{"admin", "googleapis", pd},
	} {
		_, err := callers[tc.caller].mods.CreateModules(ctx, connect.NewRequest(&modulev1.CreateModulesRequest{
			Values: []*modulev1.CreateModulesRequest_Value{{OwnerRef: ownerName(tc.owner), Name: "new-" + tc.owner + "-" + fmt.Sprint(len(tc.caller))}}}))
		check(t, tc.caller, "create", tc.owner, err, tc.want)
	}

	// ListModules only shows readable modules.
	for name, wantN := range map[string]int{"anonymous": 3, "viewer": 5, "member+read grant on alice/apriv": 6} {
		resp, err := callers[name].mods.ListModules(ctx, connect.NewRequest(&modulev1.ListModulesRequest{
			OwnerRefs: []*ownerv1.OwnerRef{ownerName("acme"), ownerName("alice"), ownerName("googleapis")}}))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range resp.Msg.GetModules() {
			if !strings.HasPrefix(m.GetName(), "new-") {
				n++
			}
		}
		if n != wantN {
			t.Errorf("%s: ListModules shows %d pre-existing modules, want %d", name, n, wantN)
		}
	}
}

func check(t *testing.T, who, action, target string, err error, want connect.Code) {
	t.Helper()
	if got := codeOf(err); got != want {
		t.Errorf("%s %s %s: got %v (%v), want %v", who, action, target, got, err, want)
	}
}

// TestContributorGrantsAndDelete: an admin grant allows settings changes and
// deletion; a write grant allows labels but not deletion.
func TestContributorGrantsAndDelete(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	createModule(t, e.mods, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("admin"), Name: "lib"})
	commit, err := e.up.Upload(ctx, connect.NewRequest(&modulev1.UploadRequest{Contents: []*modulev1.UploadRequest_Content{
		{ModuleRef: modName("admin", "lib"), Files: protoFiles("l.proto", "// l")}}}))
	if err != nil {
		t.Fatal(err)
	}
	commitID := commit.Msg.GetCommits()[0].GetId()
	writer, writerSecret := userWithRole(t, e.st, "writer", store.RoleViewer)
	boss, bossSecret := userWithRole(t, e.st, "boss", store.RoleMember)
	grant(t, e.st, "admin", "lib", writer, store.ContributorWrite)
	grant(t, e.st, "admin", "lib", boss, store.ContributorAdmin)

	wl := modulev1connect.NewLabelServiceClient(http.DefaultClient, e.url, bearer(writerSecret))
	if _, err := setLabel(t, wl, labelName("admin", "lib", "v1"), commitID); err != nil {
		t.Fatalf("write grant: set label: %v", err)
	}
	wm := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(writerSecret))
	_, err = wm.DeleteModules(ctx, connect.NewRequest(&modulev1.DeleteModulesRequest{ModuleRefs: []*modulev1.ModuleRef{modName("admin", "lib")}}))
	wantCode(t, err, connect.CodePermissionDenied)

	bm := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(bossSecret))
	pub := modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC
	if _, err := bm.UpdateModules(ctx, connect.NewRequest(&modulev1.UpdateModulesRequest{Values: []*modulev1.UpdateModulesRequest_Value{
		{ModuleRef: modName("admin", "lib"), Visibility: &pub}}})); err != nil {
		t.Fatalf("admin grant: change visibility: %v", err)
	}
	if _, err := bm.DeleteModules(ctx, connect.NewRequest(&modulev1.DeleteModulesRequest{ModuleRefs: []*modulev1.ModuleRef{modName("admin", "lib")}})); err != nil {
		t.Fatalf("admin grant: delete: %v", err)
	}
	// The grants went with the module.
	if gs, err := e.st.ListUserContributions(ctx, boss.ID); err != nil || len(gs) != 0 {
		t.Fatalf("grants after delete = %v, %v", gs, err)
	}
	// Without any write grant left, the viewer is stopped by the interceptor.
	_, err = wl.ArchiveLabels(ctx, connect.NewRequest(&modulev1.ArchiveLabelsRequest{LabelRefs: []*modulev1.LabelRef{labelName("admin", "lib", "v1")}}))
	wantCode(t, err, connect.CodePermissionDenied)
}

// TestPrivateDependencies: a readable module never reveals a private
// dependency the caller cannot read, on graph and descriptor reads and on
// push.
func TestPrivateDependencies(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	_, aliceSecret := userWithRole(t, e.st, "alice", store.RoleMember)
	_, bobSecret := userWithRole(t, e.st, "bob", store.RoleMember)
	reader, readerSecret := userWithRole(t, e.st, "reader", store.RoleViewer)

	am := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(aliceSecret))
	au := modulev1connect.NewUploadServiceClient(http.DefaultClient, e.url, bearer(aliceSecret))
	createModule(t, am, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("alice"), Name: "base"})
	createModule(t, am, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("alice"), Name: "app",
		Visibility: modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC})
	push := func(c modulev1connect.UploadServiceClient, o, m string, files []*modulev1.File, deps ...string) (string, error) {
		resp, err := c.Upload(ctx, connect.NewRequest(&modulev1.UploadRequest{Contents: []*modulev1.UploadRequest_Content{
			{ModuleRef: modName(o, m), Files: files}}, DepCommitIds: deps}))
		if err != nil {
			return "", err
		}
		return resp.Msg.GetCommits()[0].GetId(), nil
	}
	baseID, err := push(au, "alice", "base", protoFiles("base/v1/base.proto", baseProto))
	if err != nil {
		t.Fatal(err)
	}
	appID, err := push(au, "alice", "app", protoFiles("app/v1/app.proto", appProto), baseID)
	if err != nil {
		t.Fatal(err)
	}
	grant(t, e.st, "alice", "base", reader, store.ContributorRead)

	appRef := moduleResource("alice", "app")
	for name, tc := range map[string]struct {
		secret  string
		want    connect.Code
		commits int
	}{
		"anonymous":        {"", connect.CodeNotFound, 0},
		"other member":     {bobSecret, connect.CodeNotFound, 0},
		"viewer with read": {readerSecret, 0, 2},
		"owner":            {aliceSecret, 0, 2},
		"instance admin":   {e.secret, 0, 2},
	} {
		var opts []connect.ClientOption
		if tc.secret != "" {
			opts = append(opts, bearer(tc.secret))
		}
		rc := newReadClients(e.url, opts...)
		// The public module itself is downloadable by everyone.
		if _, err := download(rc.dl, &modulev1.DownloadRequest_Value{ResourceRef: appRef}); err != nil {
			t.Errorf("%s: download public module: %v", name, err)
		}
		g, err := rc.gs.GetGraph(ctx, connect.NewRequest(&modulev1.GetGraphRequest{ResourceRefs: []*modulev1.ResourceRef{appRef}}))
		check(t, name, "graph", "alice/app", err, tc.want)
		if err == nil && len(g.Msg.GetGraph().GetCommits()) != tc.commits {
			t.Errorf("%s: graph has %d commits, want %d", name, len(g.Msg.GetGraph().GetCommits()), tc.commits)
		}
		_, err = rc.fds.GetFileDescriptorSet(ctx, connect.NewRequest(&modulev1.GetFileDescriptorSetRequest{ResourceRef: refID(appID)}))
		check(t, name, "file descriptor set", "alice/app", err, tc.want)
		// The private dependency itself.
		_, err = download(rc.dl, &modulev1.DownloadRequest_Value{ResourceRef: refID(baseID)})
		check(t, name, "download", "alice/base", err, tc.want)
	}

	// Pushing with a dependency the pusher cannot read is NotFound.
	bm := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(bobSecret))
	bu := modulev1connect.NewUploadServiceClient(http.DefaultClient, e.url, bearer(bobSecret))
	createModule(t, bm, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("bob"), Name: "thief"})
	_, err = push(bu, "bob", "thief", protoFiles("app/v1/app.proto", appProto), baseID)
	wantCode(t, err, connect.CodeNotFound)
}

// TestInstanceRoleAndAdminProcedures: user roles show up in the API, can be
// filtered on, and admin-only procedures need the admin role.
func TestInstanceRoleAndAdminProcedures(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	_, memberSecret := userWithRole(t, e.st, "mem", store.RoleMember)
	userWithRole(t, e.st, "view", store.RoleViewer)

	anon := ownerv1connect.NewUserServiceClient(http.DefaultClient, e.url)
	resp, err := anon.ListUsers(ctx, connect.NewRequest(&ownerv1.ListUsersRequest{
		HasRoles: []ownerv1.UserInstanceRole{ownerv1.UserInstanceRole_USER_INSTANCE_ROLE_ADMIN}}))
	if err != nil || len(resp.Msg.GetUsers()) != 1 || resp.Msg.GetUsers()[0].GetName() != "admin" {
		t.Fatalf("ListUsers(admin) = %v, %v", resp, err)
	}
	resp, err = anon.ListUsers(ctx, connect.NewRequest(&ownerv1.ListUsersRequest{
		HasRoles: []ownerv1.UserInstanceRole{ownerv1.UserInstanceRole_USER_INSTANCE_ROLE_MEMBER}}))
	if err != nil || len(resp.Msg.GetUsers()) != 2 {
		t.Fatalf("ListUsers(member) = %v, %v", resp, err)
	}

	// Organization management is admin-only (and then still unimplemented).
	for secret, want := range map[string]connect.Code{
		"":           connect.CodeUnauthenticated,
		memberSecret: connect.CodePermissionDenied,
		e.secret:     connect.CodeUnimplemented,
	} {
		var opts []connect.ClientOption
		if secret != "" {
			opts = append(opts, bearer(secret))
		}
		oc := ownerv1connect.NewOrganizationServiceClient(http.DefaultClient, e.url, opts...)
		_, err := oc.CreateOrganizations(ctx, connect.NewRequest(&ownerv1.CreateOrganizationsRequest{
			Values: []*ownerv1.CreateOrganizationsRequest_Value{{Name: "neworg"}}}))
		wantCode(t, err, want)
	}
}
