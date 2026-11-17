// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	iamv1 "github.com/marijnkoesen/protodex/gen/buf/registry/iam/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/iam/v1/iamv1connect"
	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	ownerv1 "github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1/ownerv1connect"
	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/observability"
	"github.com/marijnkoesen/protodex/internal/server"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/trust"
	"github.com/marijnkoesen/protodex/internal/trust/trusttest"
)

// scopedToken mints a token of user u restricted to module owner/name.
func scopedToken(t *testing.T, st store.Store, u store.Owner, ownerName, name, role string) string {
	t.Helper()
	ctx := context.Background()
	o, err := st.GetOwner(ctx, ownerName)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.GetModule(ctx, o.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := auth.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	if _, err := st.CreateToken(ctx, store.Token{OwnerID: u.ID, Name: "ci", Hash: auth.HashSecret(secret), ExpiresAt: &exp,
		ModuleID: m.ID, ModuleRole: role}); err != nil {
		t.Fatal(err)
	}
	return secret
}

// TestScopedTokens: a module-scoped token is capped at its role on its
// module, sees other modules like an anonymous caller and cannot reach
// token, user or module-creation procedures — even for an instance admin.
func TestScopedTokens(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	for _, m := range []struct {
		name string
		vis  modulev1.ModuleVisibility
	}{{"api", modulev1.ModuleVisibility_MODULE_VISIBILITY_PRIVATE}, {"other", modulev1.ModuleVisibility_MODULE_VISIBILITY_PRIVATE},
		{"pub", modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC}} {
		createModule(t, e.mods, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: m.name, Visibility: m.vis})
	}
	admin, err := e.st.GetOwner(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	bot, _ := userWithRole(t, e.st, "bot", store.RoleViewer)
	grant(t, e.st, "acme", "api", bot, store.ContributorWrite)
	grant(t, e.st, "acme", "other", bot, store.ContributorAdmin)

	const (
		ok = connect.Code(0)
		nf = connect.CodeNotFound
		pd = connect.CodePermissionDenied
	)
	type row struct{ read, push, admin [3]connect.Code } // api, other, pub
	for name, tc := range map[string]struct {
		secret string
		want   row
	}{
		"admin, write on api": {scopedToken(t, e.st, admin, "acme", "api", store.ContributorWrite),
			row{read: [3]connect.Code{ok, nf, ok}, push: [3]connect.Code{ok, nf, pd}, admin: [3]connect.Code{pd, nf, pd}}},
		"admin, read on api": {scopedToken(t, e.st, admin, "acme", "api", store.ContributorRead),
			// Writes are refused by the interceptor before any module lookup.
			row{read: [3]connect.Code{ok, nf, ok}, push: [3]connect.Code{pd, pd, pd}, admin: [3]connect.Code{pd, pd, pd}}},
		"bot, write on api": {scopedToken(t, e.st, bot, "acme", "api", store.ContributorWrite),
			row{read: [3]connect.Code{ok, nf, ok}, push: [3]connect.Code{ok, nf, pd}, admin: [3]connect.Code{pd, nf, pd}}},
		// The scope never adds to the user's own permission.
		"viewer without grant, write on pub": {scopedToken(t, e.st, mustUser(t, e.st, "nogrant"), "acme", "pub", store.ContributorWrite),
			row{read: [3]connect.Code{nf, nf, ok}, push: [3]connect.Code{pd, pd, pd}, admin: [3]connect.Code{pd, pd, pd}}},
		"unknown role": {scopedToken(t, e.st, admin, "acme", "api", ""),
			row{read: [3]connect.Code{nf, nf, ok}, push: [3]connect.Code{pd, pd, pd}, admin: [3]connect.Code{pd, pd, pd}}},
	} {
		c := newCaller(e.url, tc.secret)
		for i, mod := range []string{"api", "other", "pub"} {
			ref := modName("acme", mod)
			_, err := c.mods.GetModules(ctx, connect.NewRequest(&modulev1.GetModulesRequest{ModuleRefs: []*modulev1.ModuleRef{ref}}))
			check(t, name, "read", mod, err, tc.want.read[i])
			_, err = c.up.Upload(ctx, connect.NewRequest(&modulev1.UploadRequest{Contents: []*modulev1.UploadRequest_Content{
				{ModuleRef: ref, Files: protoFiles("a.proto", fmt.Sprintf("// %s %s", name, mod))}}}))
			check(t, name, "push", mod, err, tc.want.push[i])
			desc := "by " + name
			_, err = c.mods.UpdateModules(ctx, connect.NewRequest(&modulev1.UpdateModulesRequest{Values: []*modulev1.UpdateModulesRequest_Value{
				{ModuleRef: ref, Description: &desc}}}))
			check(t, name, "update settings", mod, err, tc.want.admin[i])
		}
		_, err := c.mods.CreateModules(ctx, connect.NewRequest(&modulev1.CreateModulesRequest{
			Values: []*modulev1.CreateModulesRequest_Value{{OwnerRef: ownerName("acme"), Name: "new"}}}))
		check(t, name, "create module", "acme/new", err, pd)

		tc2 := iamv1connect.NewTokenServiceClient(http.DefaultClient, e.url, bearer(tc.secret))
		_, err = tc2.CreateTokens(ctx, connect.NewRequest(&iamv1.CreateTokensRequest{Values: []*iamv1.CreateTokensRequest_Value{{Name: "escape"}}}))
		check(t, name, "create token", "-", err, pd)
		_, err = tc2.ListTokens(ctx, connect.NewRequest(&iamv1.ListTokensRequest{}))
		check(t, name, "list tokens", "-", err, pd)

		uc := ownerv1connect.NewUserServiceClient(http.DefaultClient, e.url, bearer(tc.secret))
		_, err = uc.CreateUsers(ctx, connect.NewRequest(&ownerv1.CreateUsersRequest{Values: []*ownerv1.CreateUsersRequest_Value{{Name: "mallory"}}}))
		check(t, name, "create user", "-", err, pd)
		_, err = uc.GetCurrentUser(ctx, connect.NewRequest(&ownerv1.GetCurrentUserRequest{}))
		check(t, name, "whoami", "-", err, ok)
	}
}

func mustUser(t *testing.T, st store.Store, name string) store.Owner {
	t.Helper()
	u, _ := userWithRole(t, st, name, store.RoleViewer)
	return u
}

// TestCITrustExchangePush: a GitHub Actions style identity token is
// exchanged over HTTP for a token that pushes to its module.
func TestCITrustExchangePush(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	createModule(t, e.mods, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: "api"})
	bot, _ := userWithRole(t, e.st, "bot", store.RoleViewer)
	grant(t, e.st, "acme", "api", bot, store.ContributorWrite)

	iss := trusttest.NewIssuer(t)
	const aud = "https://registry.example.com"
	if _, err := trust.Add(ctx, e.st, trust.Spec{Name: "gha", Module: "acme/api", User: "bot", Issuer: iss.URL(), Audience: aud,
		Repository: "acme/api", Ref: "refs/heads/main"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default().CITrust
	cfg.Enabled, cfg.GitHubIssuer = true, iss.URL()
	ts := httptest.NewServer(server.Handler(slog.New(slog.DiscardHandler), 1<<20, observability.New(),
		server.WithStore(e.st), server.WithBlobs(e.blobs), server.WithBlobLeases(e.pins),
		server.WithCITrust(trust.New(cfg, e.st, slog.New(slog.DiscardHandler)))))
	t.Cleanup(ts.Close)

	exchange := func(claims map[string]any) (int, trust.Response) {
		body, _ := json.Marshal(trust.Request{Token: iss.Token(aud, time.Minute, claims), Module: "acme/api"})
		resp, err := http.Post(ts.URL+trust.ExchangePath, "application/json", strings.NewReader(string(body))) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out trust.Response
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := exchange(map[string]any{"sub": "repo:acme/api:ref:refs/heads/dev", "repository": "acme/api", "ref": "refs/heads/dev"})
	if code != http.StatusUnauthorized || out.Token != "" {
		t.Fatalf("dev branch: %d %+v", code, out)
	}
	code, out = exchange(map[string]any{"sub": "repo:acme/api:ref:refs/heads/main", "repository": "acme/api", "ref": "refs/heads/main"})
	if code != http.StatusOK {
		t.Fatalf("main branch: %d", code)
	}
	up := modulev1connect.NewUploadServiceClient(http.DefaultClient, ts.URL, bearer(out.Token))
	if _, err := up.Upload(ctx, connect.NewRequest(&modulev1.UploadRequest{Contents: []*modulev1.UploadRequest_Content{
		{ModuleRef: modName("acme", "api"), Files: protoFiles("a.proto", "// from ci")}}})); err != nil {
		t.Fatalf("push with exchanged token: %v", err)
	}
}
