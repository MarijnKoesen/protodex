// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

func TestMigrateCommand(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	for _, dir := range []string{"up", "up", "down"} {
		var out, errOut bytes.Buffer
		if err := run(context.Background(), []string{"migrate", "-db", db, dir}, &out, &errOut); err != nil {
			t.Fatalf("migrate %s: %v", dir, err)
		}
		if !strings.Contains(out.String(), "ok") {
			t.Fatalf("output = %q", out.String())
		}
	}
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"migrate", "-db", db, "sideways"}, &out, &errOut); err == nil {
		t.Fatal("want error for bad direction")
	}
	if err := run(context.Background(), []string{"migrate"}, &out, &errOut); err == nil {
		t.Fatal("want error without -db")
	}
}

func TestServeRefusedWhenDBLocked(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var out, errOut bytes.Buffer
	err = run(context.Background(), []string{"serve", "-addr", "127.0.0.1:0", "-db-path", db}, &out, &errOut)
	if !errors.Is(err, sqlite.ErrLocked) {
		t.Fatalf("serve error = %v, want ErrLocked", err)
	}
}

func TestBootstrapCommand(t *testing.T) {
	db := filepath.Join(t.TempDir(), "b.db")
	var out, errb bytes.Buffer
	if err := bootstrap(context.Background(), []string{"-db", db}, func(string) string { return "" }, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "pdx_") {
		t.Fatalf("token not printed: %q", out.String())
	}
	out.Reset()
	if err := bootstrap(context.Background(), []string{"-db", db}, func(string) string { return "" }, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "pdx_") || !strings.Contains(out.String(), "already exists") {
		t.Fatalf("second run: %q", out.String())
	}
}

func TestBootstrapOrgAndMirrors(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "b.db")
	env := func(k string) string {
		return map[string]string{"PROTODEX_MIRROR_OWNERS": "googleapis, bufbuild"}[k]
	}
	var out, errb bytes.Buffer
	if err := bootstrap(ctx, []string{"-db", db, "-org", "acme"}, env, &out, &errb); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`organization "acme"`, `mirror owner "googleapis"`, `mirror owner "bufbuild"`, "pdx_"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q lacks %q", out.String(), want)
		}
	}
	// Rerunning is a no-op.
	out.Reset()
	if err := bootstrap(ctx, []string{"-db", db, "-org", "acme"}, env, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "bootstrap: created") {
		t.Fatalf("second run created something: %q", out.String())
	}
	// A name used by another kind is refused.
	if err := bootstrap(ctx, []string{"-db", db, "-org", "googleapis"}, env, &out, &errb); err == nil {
		t.Fatal("org over mirror accepted")
	}
	if err := bootstrap(ctx, []string{"-db", db, "-org", "Bad_Name"}, env, &out, &errb); err == nil {
		t.Fatal("invalid org name accepted")
	}

	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	owners, err := s.ListOwners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range owners {
		got = append(got, fmt.Sprintf("%s:%s:%v", o.Name, o.Type, o.Mirror))
	}
	if want := "acme:organization:false admin:user:false bufbuild:organization:true googleapis:organization:true"; strings.Join(got, " ") != want {
		t.Fatalf("owners = %v, want %s", got, want)
	}
}

func TestBootstrapFailureCreatesNothing(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "b.db")
	noenv := func(string) string { return "" }
	var out, errb bytes.Buffer
	// Conflicting names in the request are rejected up front.
	if err := bootstrap(ctx, []string{"-db", db, "-user", "admin", "-mirror-owners", "admin"}, noenv, &out, &errb); err == nil {
		t.Fatal("user and mirror with the same name accepted")
	}
	// A conflict only detectable in the database rolls everything back.
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Create(ctx, s, owner.Organization, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap(ctx, []string{"-db", db, "-user", "admin", "-org", "acme", "-mirror-owners", "googleapis"}, noenv, &out, &errb); err == nil {
		t.Fatal("user over existing org accepted")
	}
	if strings.Contains(out.String(), "created") || strings.Contains(out.String(), "pdx_") {
		t.Fatalf("failed bootstrap reported success: %q", out.String())
	}
	owners, err := s.ListOwners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 1 || owners[0].Name != "admin" {
		t.Fatalf("owners after failed bootstrap = %v", owners)
	}
}

func TestOwnerCommand(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "o.db")
	noenv := func(string) string { return "" }
	run := func(args ...string) (string, error) {
		var out, errb bytes.Buffer
		err := ownerCmd(ctx, args, noenv, &out, &errb)
		return out.String(), err
	}

	out, err := run("create", "-db", db, "user", "alice", "bob")
	if err != nil || strings.Count(out, "pdx_") != 2 {
		t.Fatalf("create users: %q %v", out, err)
	}
	if out, err = run("create", "-db", db, "org", "acme"); err != nil || !strings.Contains(out, `organization "acme"`) {
		t.Fatalf("create org: %q %v", out, err)
	}
	if _, err = run("create", "-db", db, "mirror", "googleapis"); err != nil {
		t.Fatal(err)
	}
	// Duplicates are refused atomically: "carol" is not created either.
	if _, err = run("create", "-db", db, "user", "carol", "googleapis"); err == nil || !strings.Contains(err.Error(), "mirror owner") {
		t.Fatalf("duplicate create: %v", err)
	}
	if _, err = run("create", "-db", db, "team", "x"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err = run("rename", "-db", db, "acme", "acme-corp"); err != nil {
		t.Fatal(err)
	}
	if _, err = run("rename", "-db", db, "googleapis", "gapis"); err == nil {
		t.Fatal("mirror renamed")
	}
	if _, err = run("rename", "-db", db, "nobody", "x"); err == nil {
		t.Fatal("renamed unknown owner")
	}
	out, err = run("list", "-db", db)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"acme-corp", "organization", "alice", "googleapis", "mirror owner"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "carol") || strings.Contains(out, "acme ") {
		t.Fatalf("list shows stale owners: %q", out)
	}

	// Each created user's token authenticates as that user.
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	alice, err := s.GetOwner(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if toks, err := s.ListTokens(ctx, alice.ID); err != nil || len(toks) != 1 {
		t.Fatalf("alice tokens = %v %v", toks, err)
	}

	if alice.Role != store.RoleMember {
		t.Fatalf("alice role = %q, want member", alice.Role)
	}
	_ = s.Close()

	// Roles.
	if out, err = run("create", "-db", db, "-role", "viewer", "user", "vic"); err != nil || !strings.Contains(out, "(viewer)") {
		t.Fatalf("create viewer: %q %v", out, err)
	}
	if _, err = run("create", "-db", db, "-role", "admin", "org", "x-org"); err == nil {
		t.Fatal("-role accepted for an organization")
	}
	if _, err = run("create", "-db", db, "-role", "boss", "user", "x"); err == nil {
		t.Fatal("unknown role accepted")
	}
	if out, err = run("role", "-db", db, "alice", "admin"); err != nil || !strings.Contains(out, "now admin") {
		t.Fatalf("role: %q %v", out, err)
	}
	if _, err = run("role", "-db", db, "acme-corp", "admin"); err == nil {
		t.Fatal("role set on an organization")
	}
	if _, err = run("rename", "-db", db, "-role", "admin", "alice", "al"); err == nil {
		t.Fatal("-role accepted outside create")
	}
	if out, err = run("list", "-db", db); err != nil || !strings.Contains(out, "viewer") || !strings.Contains(out, "admin") {
		t.Fatalf("list lacks roles: %q %v", out, err)
	}
	s, err = sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for name, want := range map[string]string{"alice": store.RoleAdmin, "vic": store.RoleViewer, "bob": store.RoleMember} {
		if o, err := s.GetOwner(ctx, name); err != nil || o.Role != want {
			t.Fatalf("%s role = %q (%v), want %s", name, o.Role, err, want)
		}
	}

	if _, err = run("list"); err == nil {
		t.Fatal("missing -db accepted")
	}
	if _, err = run(); err == nil {
		t.Fatal("missing action accepted")
	}
}

func TestContributorCommand(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "c.db")
	noenv := func(string) string { return "" }
	run := func(args ...string) (string, error) {
		var out, errb bytes.Buffer
		err := contributorCmd(ctx, args, noenv, &out, &errb)
		return out.String(), err
	}
	if err := ownerCmd(ctx, []string{"create", "-db", db, "user", "alice"}, noenv, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"org", "mirror"} {
		name := map[string]string{"org": "acme", "mirror": "googleapis"}[kind]
		if err := ownerCmd(ctx, []string{"create", "-db", db, kind, name}, noenv, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []string{"acme", "googleapis"} {
		ow, _ := s.GetOwner(ctx, o)
		if _, err := s.CreateModule(ctx, store.Module{OwnerID: ow.ID, Name: "api", Visibility: store.VisibilityPrivate, DefaultLabel: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()

	if out, err := run("add", "-db", db, "acme/api", "alice", "write"); err != nil || !strings.Contains(out, "granted write") {
		t.Fatalf("add: %q %v", out, err)
	}
	if _, err := run("add", "-db", db, "acme/api", "alice", "admin"); err != nil {
		t.Fatal(err)
	}
	if out, err := run("list", "-db", db, "acme/api"); err != nil || !strings.Contains(out, "alice") || !strings.Contains(out, "admin") {
		t.Fatalf("list: %q %v", out, err)
	}
	for _, bad := range [][]string{
		{"add", "-db", db, "acme/api", "alice", "owner"},      // unknown role
		{"add", "-db", db, "acme/api", "acme", "read"},        // not a user
		{"add", "-db", db, "acme/nope", "alice", "read"},      // unknown module
		{"add", "-db", db, "googleapis/api", "alice", "read"}, // mirror
		{"add", "-db", db, "acme", "alice", "read"},           // malformed
		{"add", "-db", db, "acme/api", "alice"},               // arity
		{"frob", "-db", db, "acme/api"},
		{"list", "acme/api"}, // no -db
	} {
		if _, err := run(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if _, err := run("remove", "-db", db, "acme/api", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("remove", "-db", db, "acme/api", "alice"); err == nil {
		t.Fatal("removing a missing grant succeeded")
	}
	if out, err := run("list", "-db", db, "acme/api"); err != nil || strings.Contains(out, "alice") {
		t.Fatalf("list after remove: %q %v", out, err)
	}
}
