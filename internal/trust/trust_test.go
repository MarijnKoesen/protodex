// SPDX-License-Identifier: Apache-2.0

package trust_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
	"github.com/marijnkoesen/protodex/internal/trust"
)

func gh() store.TrustCredential {
	return store.TrustCredential{Name: "gha", Provider: store.TrustGitHub, Issuer: config.GitHubActionsIssuer,
		Audience: "https://pd.example.com", Repository: "acme/api", Role: store.ContributorWrite}
}

func TestValidate(t *testing.T) {
	k8s := store.TrustCredential{Name: "k", Provider: store.TrustKubernetes, Issuer: "https://k8s", Audience: "pd",
		SubjectPattern: "system:serviceaccount:ci:pusher", Role: store.ContributorRead}
	for _, ok := range []store.TrustCredential{gh(), k8s} {
		if err := trust.Validate(ok); err != nil {
			t.Errorf("Validate(%+v) = %v", ok, err)
		}
	}
	for name, mod := range map[string]func(*store.TrustCredential){
		"bad name":            func(c *store.TrustCredential) { c.Name = "a b" },
		"no audience":         func(c *store.TrustCredential) { c.Audience = " " },
		"no issuer":           func(c *store.TrustCredential) { c.Issuer = "" },
		"admin role":          func(c *store.TrustCredential) { c.Role = store.ContributorAdmin },
		"ttl too long":        func(c *store.TrustCredential) { c.TokenTTL = 2 * time.Hour },
		"negative ttl":        func(c *store.TrustCredential) { c.TokenTTL = -time.Second },
		"no repository":       func(c *store.TrustCredential) { c.Repository = "" },
		"wildcard repository": func(c *store.TrustCredential) { c.Repository = "acme/*" },
		"repository no slash": func(c *store.TrustCredential) { c.Repository = "acme" },
		"match-all subject":   func(c *store.TrustCredential) { c.SubjectPattern = "*" },
		"match-all ref":       func(c *store.TrustCredential) { c.RefPattern = "**" },
		"unknown provider":    func(c *store.TrustCredential) { c.Provider = "gitlab" },
		"k8s without subject": func(c *store.TrustCredential) {
			c.Provider, c.Repository, c.SubjectPattern = store.TrustKubernetes, "", ""
		},
		"k8s with repository": func(c *store.TrustCredential) {
			c.Provider, c.SubjectPattern = store.TrustKubernetes, "system:serviceaccount:a:b"
		},
	} {
		c := gh()
		mod(&c)
		if trust.Validate(c) == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestMatch(t *testing.T) {
	c := gh()
	c.SubjectPattern = "repo:acme/api:*"
	c.RefPattern = "refs/heads/main"
	good := trust.Claims{Subject: "repo:acme/api:ref:refs/heads/main", Repository: "acme/api", Ref: "refs/heads/main"}
	if ok, failed := trust.Match(c, good); !ok {
		t.Fatalf("Match(good) failed on %s", failed)
	}
	for name, tc := range map[string]struct {
		mod    func(*store.TrustCredential, *trust.Claims)
		failed string
	}{
		"other repo":         {func(_ *store.TrustCredential, cl *trust.Claims) { cl.Repository = "evil/api" }, "repository"},
		"repo case":          {func(_ *store.TrustCredential, cl *trust.Claims) { cl.Repository = "ACME/api" }, "repository"},
		"missing repo claim": {func(_ *store.TrustCredential, cl *trust.Claims) { cl.Repository = "" }, "repository"},
		"other branch":       {func(_ *store.TrustCredential, cl *trust.Claims) { cl.Ref = "refs/heads/dev" }, "ref"},
		"missing ref claim":  {func(_ *store.TrustCredential, cl *trust.Claims) { cl.Ref = "" }, "ref"},
		"subject":            {func(_ *store.TrustCredential, cl *trust.Claims) { cl.Subject = "repo:acme/web:ref:refs/heads/main" }, "subject"},
		"glob is anchored": {func(c *store.TrustCredential, cl *trust.Claims) {
			c.RefPattern = "refs/heads/release-*"
			cl.Ref = "refs/heads/x/refs/heads/release-1"
		}, "ref"},
		"? is one char": {func(c *store.TrustCredential, cl *trust.Claims) {
			c.RefPattern = "refs/heads/v?"
			cl.Ref = "refs/heads/v10"
		}, "ref"},
		"regexp chars are literal": {func(c *store.TrustCredential, cl *trust.Claims) {
			c.RefPattern = "refs/heads/m.in"
			cl.Ref = "refs/heads/main"
		}, "ref"},
		"no mandatory rule": {func(c *store.TrustCredential, _ *trust.Claims) { c.Repository = "" }, "credential has no mandatory rule"},
		"k8s no subject": {func(c *store.TrustCredential, _ *trust.Claims) {
			c.Provider, c.Repository, c.SubjectPattern = store.TrustKubernetes, "", ""
		}, "credential has no mandatory rule"},
	} {
		c2, cl := c, good
		tc.mod(&c2, &cl)
		ok, failed := trust.Match(c2, cl)
		if ok || failed != tc.failed {
			t.Errorf("%s: Match = %v, %q; want false, %q", name, ok, failed, tc.failed)
		}
	}
	c.RefPattern = "refs/tags/v*"
	if ok, _ := trust.Match(c, trust.Claims{Subject: good.Subject, Repository: "acme/api", Ref: "refs/tags/v1.2.3"}); !ok {
		t.Error("tag glob did not match")
	}
	c.RefPattern = "refs/heads/*"
	if ok, _ := trust.Match(c, trust.Claims{Subject: good.Subject, Repository: "acme/api", Ref: "refs/heads/feature/x"}); !ok {
		t.Error("* did not match across /")
	}
}

type fixture struct {
	st     store.Store
	module store.Module
	bot    store.Owner
}

func newFixture(t *testing.T) fixture {
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
	o, err := st.CreateOwner(ctx, store.Owner{Name: "acme", Type: store.OwnerOrganization})
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.CreateModule(ctx, store.Module{OwnerID: o.ID, Name: "api", Visibility: store.VisibilityPrivate, DefaultLabel: "main"})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := st.CreateOwner(ctx, store.Owner{Name: "ci-bot", Type: store.OwnerUser, Role: store.RoleViewer})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{st: st, module: m, bot: bot}
}

func TestAddListRemove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, err := trust.Add(ctx, f.st, trust.Spec{Name: "gha", Module: "acme/api", User: "ci-bot", Audience: "pd", Repository: "acme/api"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider != store.TrustGitHub || c.Issuer != config.GitHubActionsIssuer || c.Role != store.ContributorWrite || c.ModuleID != f.module.ID || c.UserID != f.bot.ID {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if _, err := trust.Add(ctx, f.st, trust.Spec{Name: "gha", Module: "acme/api", User: "ci-bot", Audience: "pd", Repository: "acme/api"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate: %v", err)
	}
	for name, s := range map[string]trust.Spec{
		"bad module":    {Name: "x", Module: "acme", User: "ci-bot", Audience: "pd", Repository: "acme/api"},
		"no module":     {Name: "x", Module: "acme/nope", User: "ci-bot", Audience: "pd", Repository: "acme/api"},
		"no user":       {Name: "x", Module: "acme/api", User: "nobody", Audience: "pd", Repository: "acme/api"},
		"org as user":   {Name: "x", Module: "acme/api", User: "acme", Audience: "pd", Repository: "acme/api"},
		"k8s no issuer": {Name: "x", Module: "acme/api", User: "ci-bot", Audience: "pd", Provider: store.TrustKubernetes, Subject: "system:serviceaccount:a:b"},
		"invalid rules": {Name: "x", Module: "acme/api", User: "ci-bot", Audience: "pd"},
	} {
		if _, err := trust.Add(ctx, f.st, s); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := trust.Add(ctx, f.st, trust.Spec{Name: "k8s", Module: "acme/api", User: "ci-bot", Audience: "pd", Provider: store.TrustKubernetes,
		Issuer: "https://k8s", Subject: "system:serviceaccount:ci:*", Role: store.ContributorRead, TTL: 5 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	for _, module := range []string{"", "acme/api"} {
		ls, err := trust.List(ctx, f.st, module)
		if err != nil || len(ls) != 2 || ls[0].Name != "gha" || ls[0].Module != "acme/api" || ls[0].User != "ci-bot" || ls[1].TokenTTL != 5*time.Minute {
			t.Fatalf("List(%q) = %+v, %v", module, ls, err)
		}
	}
	if _, err := trust.List(ctx, f.st, "acme/nope"); err == nil {
		t.Error("List of unknown module: want error")
	}
	if err := trust.Remove(ctx, f.st, "gha"); err != nil {
		t.Fatal(err)
	}
	if err := trust.Remove(ctx, f.st, "gha"); err == nil {
		t.Error("second remove: want error")
	}
}
