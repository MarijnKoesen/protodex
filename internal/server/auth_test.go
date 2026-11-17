// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1 "github.com/marijnkoesen/protodex/gen/buf/registry/iam/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/iam/v1/iamv1connect"
	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	ownerv1 "github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1"
	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/observability"
	"github.com/marijnkoesen/protodex/internal/server"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

type authEnv struct {
	url    string
	st     store.Store
	secret string
}

func newAuthEnv(t *testing.T) authEnv {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	secret, _, err := auth.Bootstrap(ctx, st, "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler(slog.New(slog.DiscardHandler), 1<<20, observability.New(), server.WithStore(st)))
	t.Cleanup(ts.Close)
	return authEnv{url: ts.URL, st: st, secret: secret}
}

func (e authEnv) client(bearer string) iamv1connect.TokenServiceClient {
	return iamv1connect.NewTokenServiceClient(http.DefaultClient, e.url, connect.WithInterceptors(
		connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
				if bearer != "" {
					r.Header().Set("Authorization", "Bearer "+bearer)
				}
				return next(ctx, r)
			}
		})))
}

func wantCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil || connect.CodeOf(err) != want {
		t.Fatalf("err = %v (code %v), want %v", err, connect.CodeOf(err), want)
	}
}

func TestLoginFlowAndTokenLifecycle(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	c := e.client(e.secret)

	// `buf registry login` verifies the token via the v1alpha1 AuthnService.
	getCurrentUser := func(bearer string) (int, string) {
		r, _ := http.NewRequest(http.MethodPost, e.url+"/buf.alpha.registry.v1alpha1.AuthnService/GetCurrentUser", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if status, body := getCurrentUser(e.secret); status != 200 || !strings.Contains(body, `"username":"admin"`) {
		t.Fatalf("login check: %d %s", status, body)
	}
	if status, _ := getCurrentUser("bad"); status != http.StatusUnauthorized {
		t.Fatalf("bad token login status = %d", status)
	}
	if status, _ := getCurrentUser(""); status != http.StatusUnauthorized {
		t.Fatalf("anonymous login status = %d", status)
	}

	created, err := c.CreateTokens(ctx, connect.NewRequest(&iamv1.CreateTokensRequest{
		Values: []*iamv1.CreateTokensRequest_Value{{Name: "ci-token", ExpireTime: timestamppb.New(time.Now().Add(time.Hour))}},
	}))
	if err != nil || len(created.Msg.Values) != 1 {
		t.Fatalf("create: %v", err)
	}
	v := created.Msg.Values[0]
	if v.Secret == "" || v.Secret == v.Token.Id {
		t.Fatal("no secret returned")
	}
	// The new secret works and is stored hashed.
	if _, err := e.client(v.Secret).ListTokens(ctx, connect.NewRequest(&iamv1.ListTokensRequest{})); err != nil {
		t.Fatalf("new token: %v", err)
	}
	if _, err := e.st.GetTokenByHash(ctx, v.Secret); err == nil {
		t.Fatal("secret stored in plain text")
	}

	list, err := c.ListTokens(ctx, connect.NewRequest(&iamv1.ListTokensRequest{}))
	if err != nil || len(list.Msg.Tokens) != 2 {
		t.Fatalf("list: %v %v", list, err)
	}
	byName, err := c.GetTokens(ctx, connect.NewRequest(&iamv1.GetTokensRequest{TokenRefs: []*iamv1.TokenRef{
		{Value: &iamv1.TokenRef_Name_{Name: &iamv1.TokenRef_Name{Name: "ci-token", UserRef: &ownerv1.UserRef{Value: &ownerv1.UserRef_Name{Name: "admin"}}}}}}}))
	if err != nil || byName.Msg.Tokens[0].Id != v.Token.Id {
		t.Fatalf("get by name: %v", err)
	}

	_, err = c.CreateTokens(ctx, connect.NewRequest(&iamv1.CreateTokensRequest{Values: []*iamv1.CreateTokensRequest_Value{{Name: "ci-token"}}}))
	if err != nil {
		t.Fatalf("duplicate names are allowed: %v", err)
	}
	_, err = c.GetTokens(ctx, connect.NewRequest(&iamv1.GetTokensRequest{TokenRefs: []*iamv1.TokenRef{
		{Value: &iamv1.TokenRef_Name_{Name: &iamv1.TokenRef_Name{Name: "ci-token", UserRef: &ownerv1.UserRef{Value: &ownerv1.UserRef_Name{Name: "admin"}}}}}}}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = c.CreateTokens(ctx, connect.NewRequest(&iamv1.CreateTokensRequest{Values: []*iamv1.CreateTokensRequest_Value{
		{Name: "old-token", ExpireTime: timestamppb.New(time.Now().Add(-time.Hour))}}}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = c.CreateTokens(ctx, connect.NewRequest(&iamv1.CreateTokensRequest{Values: []*iamv1.CreateTokensRequest_Value{
		{Name: "other-token", UserRef: &ownerv1.UserRef{Value: &ownerv1.UserRef_Name{Name: "someone"}}}}}))
	wantCode(t, err, connect.CodePermissionDenied)

	if _, err := c.DeleteTokens(ctx, connect.NewRequest(&iamv1.DeleteTokensRequest{TokenRefs: []*iamv1.TokenRef{
		{Value: &iamv1.TokenRef_Id{Id: v.Token.Id}}}})); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// A deleted token no longer authenticates.
	_, err = e.client(v.Secret).ListTokens(ctx, connect.NewRequest(&iamv1.ListTokensRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestRejectedCredentials(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	req := func() *connect.Request[iamv1.ListTokensRequest] {
		return connect.NewRequest(&iamv1.ListTokensRequest{})
	}

	_, err := e.client("").ListTokens(ctx, req())
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = e.client("garbage").ListTokens(ctx, req())
	wantCode(t, err, connect.CodeUnauthenticated)
	unknown, _ := auth.GenerateSecret()
	_, err = e.client(unknown).ListTokens(ctx, req())
	wantCode(t, err, connect.CodeUnauthenticated)

	// Expired token.
	owner, _ := e.st.GetOwner(ctx, "admin")
	secret, _ := auth.GenerateSecret()
	past := time.Now().Add(-time.Minute)
	if _, err := e.st.CreateToken(ctx, store.Token{OwnerID: owner.ID, Name: "exp", Hash: auth.HashSecret(secret), ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	_, err = e.client(secret).ListTokens(ctx, req())
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestAnonymousOnlyForReads(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	mc := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url)
	// Anonymous read reaches the (unimplemented) handler.
	_, err := mc.GetModules(ctx, connect.NewRequest(&modulev1.GetModulesRequest{}))
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		t.Fatalf("anonymous read rejected: %v", err)
	}
	// Anonymous write does not.
	_, err = mc.CreateModules(ctx, connect.NewRequest(&modulev1.CreateModulesRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
}
