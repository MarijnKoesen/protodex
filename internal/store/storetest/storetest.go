// SPDX-License-Identifier: Apache-2.0

// Package storetest is the backend-agnostic conformance suite for store.Store.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marijnkoesen/protodex/internal/store"
)

// Run runs the suite. newStore must return a fresh, migrated, empty store.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	tests := map[string]func(*testing.T, store.Store){
		"OwnersAndModules":      testOwnersAndModules,
		"OwnerUniqueness":       testOwnerUniqueness,
		"OwnerRename":           testOwnerRename,
		"CommitsLabelsDeps":     testCommitsLabelsDeps,
		"BlobsAndManifests":     testBlobsAndManifests,
		"Tokens":                testTokens,
		"Identities":            testIdentities,
		"TxCommitRollback":      testTx,
		"DeleteModuleCascades":  testDeleteModule,
		"ConcurrentWriters":     testConcurrent,
		"LabelCrossModule":      testLabelCrossModule,
		"TxPanicReleasesWriter": testTxPanic,
		"SetManifestAtomic":     testManifestAtomic,
		"ModuleUpdate":          testModuleUpdate,
		"LabelIdentity":         testLabelIdentity,
		"BlobGC":                testBlobGC,
		"RolesAndContributors":  testRolesAndContributors,
		"AuditAndWebhooks":      testAuditAndWebhooks,
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })
			defer func() {
				if r := recover(); r != nil {
					f, ok := r.(mustFailure)
					if !ok {
						panic(r)
					}
					t.Fatal(f.err)
				}
			}()
			fn(t, s)
		})
	}
}

type mustFailure struct{ err error }

// must unwraps a (value, error) pair; a non-nil error aborts the running
// test case (recovered in Run).
func must[T any](v T, err error) T {
	if err != nil {
		panic(mustFailure{err})
	}
	return v
}

func is(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got error %v, want %v", err, want)
	}
}

func seed(t *testing.T, s store.Querier) (store.Owner, store.Module) {
	t.Helper()
	ctx := context.Background()
	o := must(s.CreateOwner(ctx, store.Owner{Name: "acme", Type: "organization"}))
	m := must(s.CreateModule(ctx, store.Module{OwnerID: o.ID, Name: "api", Visibility: "public", DefaultLabel: "main"}))
	return o, m
}

func testOwnersAndModules(t *testing.T, s store.Store) {
	ctx := context.Background()
	o, m := seed(t, s)
	if o.ID == "" || m.ID == "" || o.CreatedAt.IsZero() {
		t.Fatalf("ids/timestamps not filled: %+v %+v", o, m)
	}
	got := must(s.GetOwner(ctx, "acme"))
	if got.ID != o.ID || got.Type != "organization" || !got.CreatedAt.Equal(o.CreatedAt) {
		t.Fatalf("GetOwner = %+v, want %+v", got, o)
	}
	_, err := s.GetOwner(ctx, "nope")
	is(t, err, store.ErrNotFound)
	_, err = s.CreateOwner(ctx, store.Owner{Name: "acme", Type: "user"})
	is(t, err, store.ErrConflict)
	_, err = s.CreateOwner(ctx, store.Owner{Name: "bad", Type: "robot"})
	is(t, err, store.ErrConflict)
	_, err = s.CreateModule(ctx, store.Module{OwnerID: o.ID, Name: "api", Visibility: "public", DefaultLabel: "main"})
	is(t, err, store.ErrConflict)
	_, err = s.CreateModule(ctx, store.Module{OwnerID: "missing", Name: "x", Visibility: "public", DefaultLabel: "main"})
	is(t, err, store.ErrConflict)

	must(s.CreateModule(ctx, store.Module{OwnerID: o.ID, Name: "abc", Visibility: "private", DefaultLabel: "main"}))
	ms := must(s.ListModules(ctx, o.ID))
	if len(ms) != 2 || ms[0].Name != "abc" || ms[1].Name != "api" {
		t.Fatalf("ListModules = %+v", ms)
	}
	if gm := must(s.GetModule(ctx, o.ID, "api")); gm.ID != m.ID {
		t.Fatalf("GetModule = %+v", gm)
	}
	if os := must(s.ListOwners(ctx)); len(os) != 1 {
		t.Fatalf("ListOwners = %+v", os)
	}
}

func testOwnerUniqueness(t *testing.T, s store.Store) {
	ctx := context.Background()
	u := must(s.CreateOwner(ctx, store.Owner{Name: "alice", Type: store.OwnerUser}))
	if u.UpdatedAt.IsZero() || !u.UpdatedAt.Equal(u.CreatedAt) || u.Mirror {
		t.Fatalf("CreateOwner = %+v", u)
	}
	m := must(s.CreateOwner(ctx, store.Owner{Name: "googleapis", Type: store.OwnerOrganization, Mirror: true}))
	// Users, organizations and mirrors share one namespace.
	for _, o := range []store.Owner{
		{Name: "alice", Type: store.OwnerOrganization},
		{Name: "alice", Type: store.OwnerUser},
		{Name: "googleapis", Type: store.OwnerUser},
		{Name: "googleapis", Type: store.OwnerOrganization},
		{Name: "googleapis", Type: store.OwnerOrganization, Mirror: true},
		{ID: u.ID, Name: "bob", Type: store.OwnerUser}, // duplicate id
	} {
		_, err := s.CreateOwner(ctx, o)
		is(t, err, store.ErrConflict)
	}
	// Only organizations can be mirrors.
	_, err := s.CreateOwner(ctx, store.Owner{Name: "carol", Type: store.OwnerUser, Mirror: true})
	is(t, err, store.ErrConflict)

	got := must(s.GetOwnerByID(ctx, m.ID))
	if got.Name != "googleapis" || !got.Mirror || got.Type != store.OwnerOrganization || !got.UpdatedAt.Equal(m.UpdatedAt) {
		t.Fatalf("GetOwnerByID = %+v, want %+v", got, m)
	}
	if got := must(s.GetOwner(ctx, "googleapis")); !got.Mirror {
		t.Fatalf("GetOwner lost mirror flag: %+v", got)
	}
	_, err = s.GetOwnerByID(ctx, "missing")
	is(t, err, store.ErrNotFound)
	if os := must(s.ListOwners(ctx)); len(os) != 2 || os[0].Name != "alice" || os[1].Name != "googleapis" {
		t.Fatalf("ListOwners = %+v", os)
	}
}

func testOwnerRename(t *testing.T, s store.Store) {
	ctx := context.Background()
	o, m := seed(t, s)
	u := must(s.CreateOwner(ctx, store.Owner{Name: "alice", Type: store.OwnerUser}))
	mirror := must(s.CreateOwner(ctx, store.Owner{Name: "googleapis", Type: store.OwnerOrganization, Mirror: true}))
	tok := must(s.CreateToken(ctx, store.Token{OwnerID: u.ID, Name: "t", Hash: "h"}))

	renamed := must(s.RenameOwner(ctx, o.ID, "acme-corp"))
	if renamed.ID != o.ID || renamed.Name != "acme-corp" || renamed.Type != o.Type ||
		!renamed.CreatedAt.Equal(o.CreatedAt) || renamed.UpdatedAt.Before(o.UpdatedAt) {
		t.Fatalf("RenameOwner = %+v (was %+v)", renamed, o)
	}
	// The old name is free and gone; modules follow the owner id.
	_, err := s.GetOwner(ctx, "acme")
	is(t, err, store.ErrNotFound)
	if got := must(s.GetOwner(ctx, "acme-corp")); got.ID != o.ID {
		t.Fatalf("GetOwner(new name) = %+v", got)
	}
	if gm := must(s.GetModule(ctx, o.ID, "api")); gm.ID != m.ID {
		t.Fatalf("module lost after rename: %+v", gm)
	}

	// Taken names conflict, whichever type holds them.
	_, err = s.RenameOwner(ctx, o.ID, "alice")
	is(t, err, store.ErrConflict)
	_, err = s.RenameOwner(ctx, u.ID, "googleapis")
	is(t, err, store.ErrConflict)
	// Mirrors are pinned to their upstream name.
	_, err = s.RenameOwner(ctx, mirror.ID, "gapis")
	is(t, err, store.ErrConflict)
	if got := must(s.GetOwnerByID(ctx, mirror.ID)); got.Name != "googleapis" {
		t.Fatalf("mirror renamed: %+v", got)
	}
	_, err = s.RenameOwner(ctx, "missing", "x")
	is(t, err, store.ErrNotFound)

	// Renaming to the current name is a no-op; the old name can be reused.
	must(s.RenameOwner(ctx, u.ID, "alice"))
	must(s.CreateOwner(ctx, store.Owner{Name: "acme", Type: store.OwnerOrganization}))

	// A renamed user's tokens still authenticate as that user.
	ru := must(s.RenameOwner(ctx, u.ID, "alice2"))
	if got := must(s.GetTokenByHash(ctx, "h")); got.ID != tok.ID || got.OwnerID != ru.ID {
		t.Fatalf("token after rename = %+v", got)
	}

	// Renames inside a rolled-back transaction do not stick.
	boom := errors.New("boom")
	err = s.WithTx(ctx, func(q store.Querier) error {
		if got := must(q.RenameOwner(ctx, u.ID, "temp")); got.Name != "temp" {
			t.Errorf("in-tx rename = %+v", got)
		}
		return boom
	})
	is(t, err, boom)
	if got := must(s.GetOwnerByID(ctx, u.ID)); got.Name != "alice2" {
		t.Fatalf("rolled-back rename visible: %+v", got)
	}
}

func testCommitsLabelsDeps(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, m := seed(t, s)
	base := time.Now().Add(-time.Hour)
	c1 := must(s.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: "b5:aa", CreatedAt: base}))
	c2 := must(s.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: "b5:bb", CreatedAt: base.Add(time.Minute)}))
	cs := must(s.ListCommits(ctx, m.ID))
	if len(cs) != 2 || cs[0].ID != c2.ID || cs[1].ID != c1.ID {
		t.Fatalf("ListCommits not newest-first: %+v", cs)
	}
	if got := must(s.GetCommit(ctx, m.ID, c1.ID)); got.Digest != "b5:aa" {
		t.Fatalf("GetCommit = %+v", got)
	}
	_, err := s.GetCommit(ctx, "other-module", c1.ID)
	is(t, err, store.ErrNotFound)
	_, err = s.CreateCommit(ctx, store.Commit{ModuleID: "missing", Digest: "x"})
	is(t, err, store.ErrConflict)

	must(s.SetLabel(ctx, store.Label{ModuleID: m.ID, Name: "main", CommitID: c1.ID}))
	must(s.SetLabel(ctx, store.Label{ModuleID: m.ID, Name: "main", CommitID: c2.ID})) // move
	l := must(s.GetLabel(ctx, m.ID, "main"))
	if l.CommitID != c2.ID || l.Archived {
		t.Fatalf("label = %+v", l)
	}
	_, err = s.SetLabel(ctx, store.Label{ModuleID: m.ID, Name: "bad", CommitID: "missing"})
	is(t, err, store.ErrConflict)
	if err := s.ArchiveLabel(ctx, m.ID, "main", true); err != nil {
		t.Fatal(err)
	}
	if !must(s.GetLabel(ctx, m.ID, "main")).Archived {
		t.Fatal("label not archived")
	}
	is(t, s.ArchiveLabel(ctx, m.ID, "nope", true), store.ErrNotFound)
	_, err = s.GetLabel(ctx, m.ID, "nope")
	is(t, err, store.ErrNotFound)
	if ls := must(s.ListLabels(ctx, m.ID)); len(ls) != 1 {
		t.Fatalf("ListLabels = %+v", ls)
	}

	deps := []store.CommitDep{
		{CommitID: c2.ID, DepOwner: "z", DepModule: "lib", DepCommitID: "c9", DepDigest: "b5:cc"},
		{CommitID: c2.ID, DepRegistry: "buf.build", DepOwner: "a", DepModule: "types", DepCommitID: "c8"},
	}
	if err := s.AddCommitDeps(ctx, deps); err != nil {
		t.Fatal(err)
	}
	got := must(s.ListCommitDeps(ctx, c2.ID))
	if len(got) != 2 || got[0].DepRegistry != "" || got[1].DepRegistry != "buf.build" {
		t.Fatalf("ListCommitDeps = %+v", got)
	}
	is(t, s.AddCommitDeps(ctx, deps[:1]), store.ErrConflict)
	is(t, s.AddCommitDeps(ctx, []store.CommitDep{{CommitID: "missing", DepOwner: "a", DepModule: "b"}}), store.ErrConflict)
	if got := must(s.ListCommitDeps(ctx, c1.ID)); len(got) != 0 {
		t.Fatalf("unexpected deps %+v", got)
	}
}

func testBlobsAndManifests(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, m := seed(t, s)
	c := must(s.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: "b5:aa"}))
	b := store.Blob{Digest: "sha256:1", Data: []byte("syntax = \"proto3\";")}
	for range 2 { // idempotent
		if err := s.PutBlob(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	got := must(s.GetBlob(ctx, b.Digest))
	if string(got.Data) != string(b.Data) || got.Size != int64(len(b.Data)) {
		t.Fatalf("GetBlob = %+v", got)
	}
	_, err := s.GetBlob(ctx, "sha256:none")
	is(t, err, store.ErrNotFound)

	es := []store.ManifestEntry{{Path: "b.proto", BlobDigest: b.Digest}, {Path: "a.proto", BlobDigest: b.Digest}}
	if err := s.SetManifest(ctx, c.ID, es); err != nil {
		t.Fatal(err)
	}
	m1 := must(s.GetManifest(ctx, c.ID))
	if len(m1) != 2 || m1[0].Path != "a.proto" || m1[0].CommitID != c.ID {
		t.Fatalf("GetManifest = %+v", m1)
	}
	if err := s.SetManifest(ctx, c.ID, es[:1]); err != nil { // replaces
		t.Fatal(err)
	}
	if m2 := must(s.GetManifest(ctx, c.ID)); len(m2) != 1 {
		t.Fatalf("manifest not replaced: %+v", m2)
	}
	is(t, s.SetManifest(ctx, c.ID, []store.ManifestEntry{{Path: "x", BlobDigest: "sha256:none"}}), store.ErrConflict)
}

func testBlobGC(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, m := seed(t, s)
	c := must(s.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: "b5:aa"}))
	for _, d := range []string{"sha256:2", "sha256:1", "sha256:3"} {
		if err := s.PutBlob(ctx, store.Blob{Digest: d, Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	es := []store.ManifestEntry{{Path: "a.proto", BlobDigest: "sha256:1"}, {Path: "b.proto", BlobDigest: "sha256:1"},
		{Path: "c.proto", BlobDigest: "sha256:3"}}
	if err := s.SetManifest(ctx, c.ID, es); err != nil {
		t.Fatal(err)
	}
	if got := must(s.ListBlobDigests(ctx)); !slices.Equal(got, []string{"sha256:1", "sha256:2", "sha256:3"}) {
		t.Fatalf("ListBlobDigests = %v", got)
	}
	if got := must(s.ListReferencedBlobDigests(ctx)); !slices.Equal(got, []string{"sha256:1", "sha256:3"}) {
		t.Fatalf("ListReferencedBlobDigests = %v", got)
	}
	if !must(s.BlobReferenced(ctx, "sha256:1")) || must(s.BlobReferenced(ctx, "sha256:2")) {
		t.Fatal("BlobReferenced wrong")
	}
	is(t, s.DeleteBlob(ctx, "sha256:1"), store.ErrConflict)
	if err := s.DeleteBlob(ctx, "sha256:2"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBlob(ctx, "sha256:missing"); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetBlob(ctx, "sha256:2")
	is(t, err, store.ErrNotFound)
	// Inside a transaction the check sees the transaction's own manifest.
	err = s.WithTx(ctx, func(q store.Querier) error {
		if err := q.SetManifest(ctx, c.ID, es[:1]); err != nil {
			return err
		}
		if ref, err := q.BlobReferenced(ctx, "sha256:3"); err != nil || ref {
			t.Errorf("BlobReferenced in tx = %v, %v", ref, err)
		}
		return q.DeleteBlob(ctx, "sha256:3")
	})
	if err != nil {
		t.Fatal(err)
	}
	// Deleting the module drops its manifests: its blobs become unreferenced.
	if err := s.DeleteModule(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if got := must(s.ListReferencedBlobDigests(ctx)); len(got) != 0 {
		t.Fatalf("referenced after module delete = %v", got)
	}
	if err := s.DeleteBlob(ctx, "sha256:1"); err != nil {
		t.Fatal(err)
	}
}

func testTokens(t *testing.T, s store.Store) {
	ctx := context.Background()
	o, _ := seed(t, s)
	exp := time.Now().Add(time.Hour)
	tok := must(s.CreateToken(ctx, store.Token{OwnerID: o.ID, Name: "ci", Hash: "h1", ExpiresAt: &exp}))
	got := must(s.GetTokenByHash(ctx, "h1"))
	if got.ID != tok.ID || got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp.UTC().Truncate(time.Microsecond)) || got.LastUsedAt != nil {
		t.Fatalf("token = %+v", got)
	}
	_, err := s.CreateToken(ctx, store.Token{OwnerID: o.ID, Name: "dup", Hash: "h1"})
	is(t, err, store.ErrConflict)
	at := time.Now()
	if err := s.TouchToken(ctx, tok.ID, at); err != nil {
		t.Fatal(err)
	}
	if g := must(s.GetTokenByHash(ctx, "h1")); g.LastUsedAt == nil {
		t.Fatal("last used not set")
	}
	if ts := must(s.ListTokens(ctx, o.ID)); len(ts) != 1 {
		t.Fatalf("ListTokens = %+v", ts)
	}
	if err := s.DeleteToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	is(t, s.DeleteToken(ctx, tok.ID), store.ErrNotFound)
	_, err = s.GetTokenByHash(ctx, "h1")
	is(t, err, store.ErrNotFound)
}

func testIdentities(t *testing.T, s store.Store) {
	ctx := context.Background()
	o, _ := seed(t, s)
	_, err := s.GetIdentity(ctx, "https://idp", "sub1")
	is(t, err, store.ErrNotFound)
	id := must(s.CreateIdentity(ctx, store.Identity{Issuer: "https://idp", Subject: "sub1", OwnerID: o.ID, Email: "a@x.io", Role: "member"}))
	_, err = s.CreateIdentity(ctx, id)
	is(t, err, store.ErrConflict)
	_, err = s.CreateIdentity(ctx, store.Identity{Issuer: "https://idp", Subject: "sub2", OwnerID: "nope", Role: "member"})
	is(t, err, store.ErrConflict)
	id.Email, id.Role, id.LastLoginAt = "b@x.io", "admin", time.Now().Add(time.Minute)
	if err := s.UpdateIdentity(ctx, id); err != nil {
		t.Fatal(err)
	}
	got := must(s.GetIdentity(ctx, "https://idp", "sub1"))
	if got.Email != "b@x.io" || got.Role != "admin" || got.OwnerID != o.ID || !got.LastLoginAt.After(got.CreatedAt) {
		t.Fatalf("identity = %+v", got)
	}
	is(t, s.UpdateIdentity(ctx, store.Identity{Issuer: "https://idp", Subject: "zzz"}), store.ErrNotFound)
}

func testTx(t *testing.T, s store.Store) {
	ctx := context.Background()
	boom := errors.New("boom")
	err := s.WithTx(ctx, func(q store.Querier) error {
		must(q.CreateOwner(ctx, store.Owner{Name: "gone", Type: "user"}))
		if _, err := q.GetOwner(ctx, "gone"); err != nil { // sees own writes
			return err
		}
		return boom
	})
	is(t, err, boom)
	_, err = s.GetOwner(ctx, "gone")
	is(t, err, store.ErrNotFound)

	err = s.WithTx(ctx, func(q store.Querier) error {
		_, err := q.CreateOwner(ctx, store.Owner{Name: "kept", Type: "user"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	must(s.GetOwner(ctx, "kept"))
}

func testDeleteModule(t *testing.T, s store.Store) {
	ctx := context.Background()
	o, m := seed(t, s)
	c := must(s.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: "b5:aa"}))
	must(s.SetLabel(ctx, store.Label{ModuleID: m.ID, Name: "main", CommitID: c.ID}))
	if err := s.DeleteModule(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetModule(ctx, o.ID, "api")
	is(t, err, store.ErrNotFound)
	if cs := must(s.ListCommits(ctx, m.ID)); len(cs) != 0 {
		t.Fatalf("commits remain: %+v", cs)
	}
	is(t, s.DeleteModule(ctx, m.ID), store.ErrNotFound)
}

func testConcurrent(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, m := seed(t, s)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- s.WithTx(ctx, func(q store.Querier) error {
				_, err := q.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: fmt.Sprintf("b5:%d", i)})
				return err
			})
		}()
		go func() {
			defer wg.Done()
			_, err := s.ListCommits(ctx, m.ID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if cs := must(s.ListCommits(ctx, m.ID)); len(cs) != 20 {
		t.Fatalf("got %d commits, want 20", len(cs))
	}
}

func testLabelCrossModule(t *testing.T, s store.Store) {
	ctx := context.Background()
	o, a := seed(t, s)
	b := must(s.CreateModule(ctx, store.Module{OwnerID: o.ID, Name: "other", Visibility: "public", DefaultLabel: "main"}))
	cb := must(s.CreateCommit(ctx, store.Commit{ModuleID: b.ID, Digest: "b5:aa"}))
	_, err := s.SetLabel(ctx, store.Label{ModuleID: a.ID, Name: "main", CommitID: cb.ID})
	is(t, err, store.ErrConflict)
}

func testTxPanic(t *testing.T, s store.Store) {
	ctx := context.Background()
	func() {
		defer func() { _ = recover() }()
		_ = s.WithTx(ctx, func(q store.Querier) error {
			must(q.CreateOwner(ctx, store.Owner{Name: "p", Type: "user"}))
			panic("boom")
		})
	}()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	must(s.CreateOwner(wctx, store.Owner{Name: "after", Type: "user"}))
	_, err := s.GetOwner(ctx, "p")
	is(t, err, store.ErrNotFound)
}

func testManifestAtomic(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, m := seed(t, s)
	c := must(s.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: "b5:aa"}))
	if err := s.PutBlob(ctx, store.Blob{Digest: "sha256:1", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	good := []store.ManifestEntry{{Path: "a.proto", BlobDigest: "sha256:1"}}
	if err := s.SetManifest(ctx, c.ID, good); err != nil {
		t.Fatal(err)
	}
	bad := []store.ManifestEntry{{Path: "a.proto", BlobDigest: "sha256:1"}, {Path: "b.proto", BlobDigest: "sha256:none"}}
	is(t, s.SetManifest(ctx, c.ID, bad), store.ErrConflict)
	if got := must(s.GetManifest(ctx, c.ID)); len(got) != 1 || got[0].Path != "a.proto" {
		t.Fatalf("manifest not preserved after failed replace: %+v", got)
	}
}

func testModuleUpdate(t *testing.T, s store.Store) {
	ctx := context.Background()
	o, m := seed(t, s)
	if !m.UpdatedAt.Equal(m.CreatedAt) || m.Deprecated || m.Description != "" {
		t.Fatalf("CreateModule = %+v", m)
	}
	other := must(s.CreateOwner(ctx, store.Owner{Name: "zeta", Type: store.OwnerUser}))
	must(s.CreateModule(ctx, store.Module{OwnerID: other.ID, Name: "b", Visibility: store.VisibilityPrivate, DefaultLabel: "main"}))
	if all := must(s.ListAllModules(ctx)); len(all) != 2 {
		t.Fatalf("ListAllModules = %+v", all)
	}

	m.Visibility, m.DefaultLabel, m.Description, m.URL, m.Deprecated = store.VisibilityPrivate, "dev", "d", "https://x.test", true
	m.UpdatedAt = m.CreatedAt.Add(time.Hour)
	got := must(s.UpdateModule(ctx, m))
	if got.Visibility != store.VisibilityPrivate || got.DefaultLabel != "dev" || got.Description != "d" ||
		got.URL != "https://x.test" || !got.Deprecated || !got.UpdatedAt.Equal(m.UpdatedAt) ||
		!got.CreatedAt.Equal(m.CreatedAt) || got.Name != "api" || got.OwnerID != o.ID {
		t.Fatalf("UpdateModule = %+v", got)
	}
	if byID := must(s.GetModuleByID(ctx, m.ID)); byID != got {
		t.Fatalf("GetModuleByID = %+v, want %+v", byID, got)
	}
	_, err := s.UpdateModule(ctx, store.Module{ID: "missing", Visibility: store.VisibilityPublic})
	is(t, err, store.ErrNotFound)
	_, err = s.GetModuleByID(ctx, "missing")
	is(t, err, store.ErrNotFound)
}

func testLabelIdentity(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, m := seed(t, s)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	c1 := must(s.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: "b5:aa", CreatedAt: base}))
	c2 := must(s.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: "b5:bb", CreatedAt: base.Add(time.Minute)}))
	if got := must(s.GetCommitByID(ctx, c2.ID)); got.ModuleID != m.ID {
		t.Fatalf("GetCommitByID = %+v", got)
	}
	_, err := s.GetCommitByID(ctx, "missing")
	is(t, err, store.ErrNotFound)

	l1 := must(s.SetLabel(ctx, store.Label{ModuleID: m.ID, Name: "main", CommitID: c1.ID, UpdatedBy: "u1", UpdatedAt: base}))
	if l1.ID == "" || !l1.CreatedAt.Equal(base) || l1.UpdatedBy != "u1" || l1.ArchivedAt != nil {
		t.Fatalf("SetLabel (create) = %+v", l1)
	}
	// Moving keeps id and creation time.
	l2 := must(s.SetLabel(ctx, store.Label{ModuleID: m.ID, Name: "main", CommitID: c2.ID, UpdatedBy: "u2", UpdatedAt: base.Add(time.Minute)}))
	if l2.ID != l1.ID || !l2.CreatedAt.Equal(base) || l2.CommitID != c2.ID || l2.UpdatedBy != "u2" ||
		!l2.UpdatedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("SetLabel (move) = %+v, first %+v", l2, l1)
	}
	if got := must(s.GetLabelByID(ctx, l1.ID)); got.Name != "main" || got.CommitID != c2.ID {
		t.Fatalf("GetLabelByID = %+v", got)
	}
	_, err = s.GetLabelByID(ctx, "missing")
	is(t, err, store.ErrNotFound)

	if err := s.ArchiveLabel(ctx, m.ID, "main", true); err != nil {
		t.Fatal(err)
	}
	if got := must(s.GetLabel(ctx, m.ID, "main")); !got.Archived || got.ArchivedAt == nil {
		t.Fatalf("archived label = %+v", got)
	}
	if err := s.ArchiveLabel(ctx, m.ID, "main", false); err != nil {
		t.Fatal(err)
	}
	if got := must(s.GetLabel(ctx, m.ID, "main")); got.Archived || got.ArchivedAt != nil {
		t.Fatalf("unarchived label = %+v", got)
	}
	// Setting an archived label to unarchived through SetLabel also clears it.
	if err := s.ArchiveLabel(ctx, m.ID, "main", true); err != nil {
		t.Fatal(err)
	}
	if got := must(s.SetLabel(ctx, store.Label{ModuleID: m.ID, Name: "main", CommitID: c2.ID})); got.Archived || got.ArchivedAt != nil {
		t.Fatalf("SetLabel after archive = %+v", got)
	}
}

func testRolesAndContributors(t *testing.T, s store.Store) {
	ctx := context.Background()
	org, m := seed(t, s)
	if org.Role != "" {
		t.Fatalf("organization role = %q, want empty", org.Role)
	}
	u := must(s.CreateOwner(ctx, store.Owner{Name: "alice", Type: store.OwnerUser}))
	if u.Role != store.RoleMember || must(s.GetOwner(ctx, "alice")).Role != store.RoleMember {
		t.Fatalf("default user role = %q, want member", u.Role)
	}
	a := must(s.CreateOwner(ctx, store.Owner{Name: "root", Type: store.OwnerUser, Role: store.RoleAdmin}))
	if must(s.GetOwnerByID(ctx, a.ID)).Role != store.RoleAdmin {
		t.Fatal("admin role not stored")
	}
	if err := s.SetOwnerRole(ctx, u.ID, store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	if got := must(s.GetOwnerByID(ctx, u.ID)); got.Role != store.RoleViewer {
		t.Fatalf("role after SetOwnerRole = %q", got.Role)
	}
	is(t, s.SetOwnerRole(ctx, org.ID, store.RoleAdmin), store.ErrNotFound)
	is(t, s.SetOwnerRole(ctx, "missing", store.RoleAdmin), store.ErrNotFound)
	is(t, s.SetOwnerRole(ctx, u.ID, "superuser"), store.ErrConflict)
	_, err := s.CreateOwner(ctx, store.Owner{Name: "org2", Type: store.OwnerOrganization, Role: store.RoleAdmin})
	is(t, err, store.ErrConflict)

	c := must(s.SetModuleContributor(ctx, store.ModuleContributor{ModuleID: m.ID, UserID: u.ID, Role: store.ContributorRead}))
	if c.Role != store.ContributorRead || c.CreatedAt.IsZero() {
		t.Fatalf("SetModuleContributor = %+v", c)
	}
	c2 := must(s.SetModuleContributor(ctx, store.ModuleContributor{ModuleID: m.ID, UserID: u.ID, Role: store.ContributorWrite,
		UpdatedAt: c.UpdatedAt.Add(time.Second)}))
	if c2.Role != store.ContributorWrite || !c2.CreatedAt.Equal(c.CreatedAt) {
		t.Fatalf("changed grant = %+v, created %v", c2, c.CreatedAt)
	}
	must(s.SetModuleContributor(ctx, store.ModuleContributor{ModuleID: m.ID, UserID: a.ID, Role: store.ContributorAdmin}))
	if got := must(s.GetModuleContributor(ctx, m.ID, u.ID)); got.Role != store.ContributorWrite {
		t.Fatalf("GetModuleContributor = %+v", got)
	}
	_, err = s.GetModuleContributor(ctx, m.ID, org.ID)
	is(t, err, store.ErrNotFound)
	if cs := must(s.ListModuleContributors(ctx, m.ID)); len(cs) != 2 {
		t.Fatalf("ListModuleContributors = %+v", cs)
	}
	if cs := must(s.ListUserContributions(ctx, u.ID)); len(cs) != 1 || cs[0].ModuleID != m.ID {
		t.Fatalf("ListUserContributions = %+v", cs)
	}
	for _, bad := range []store.ModuleContributor{
		{ModuleID: "missing", UserID: u.ID, Role: store.ContributorRead},
		{ModuleID: m.ID, UserID: "missing", Role: store.ContributorRead},
		{ModuleID: m.ID, UserID: u.ID, Role: "owner"},
	} {
		_, err := s.SetModuleContributor(ctx, bad)
		is(t, err, store.ErrConflict)
	}
	if err := s.DeleteModuleContributor(ctx, m.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	is(t, s.DeleteModuleContributor(ctx, m.ID, u.ID), store.ErrNotFound)
	// Grants go away with their module.
	if err := s.DeleteModule(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if cs := must(s.ListUserContributions(ctx, a.ID)); len(cs) != 0 {
		t.Fatalf("grants after module delete = %+v", cs)
	}
}

func testAuditAndWebhooks(t *testing.T, s store.Store) {
	ctx := context.Background()
	var ids []string
	for _, typ := range []string{"a.one", "a.two", "b.one"} {
		e := must(s.CreateAuditEvent(ctx, store.AuditEvent{Type: typ, Subject: "x", Data: `{"k":1}`}))
		ids = append(ids, e.ID)
	}
	if ids[0] >= ids[1] || ids[1] >= ids[2] {
		t.Fatalf("event ids are not ordered: %v", ids)
	}
	if got := must(s.ListAuditEvents(ctx, "", "", 10)); len(got) != 3 || got[0].ID != ids[0] || got[0].Data != `{"k":1}` {
		t.Fatalf("ListAuditEvents = %+v", got)
	}
	if got := must(s.ListAuditEvents(ctx, ids[0], "", 1)); len(got) != 1 || got[0].ID != ids[1] {
		t.Fatalf("paged = %+v", got)
	}
	if got := must(s.ListAuditEvents(ctx, "", "b.one", 10)); len(got) != 1 || got[0].ID != ids[2] {
		t.Fatalf("filtered = %+v", got)
	}
	_, err := s.GetAuditEvent(ctx, "nope")
	is(t, err, store.ErrNotFound)

	w := must(s.CreateWebhook(ctx, store.Webhook{URL: "https://example.com/h", Secret: "s", EventTypes: []string{"a.*", "b.one"}, Active: true}))
	if g := must(s.GetWebhook(ctx, w.ID)); g.URL != w.URL || len(g.EventTypes) != 2 || !g.Active || g.Secret != "s" {
		t.Fatalf("webhook = %+v", g)
	}
	w.URL, w.Active, w.EventTypes = "https://example.com/other", false, nil
	if u := must(s.UpdateWebhook(ctx, w)); u.URL != w.URL || u.Active || len(u.EventTypes) != 0 {
		t.Fatalf("updated = %+v", u)
	}
	_, err = s.UpdateWebhook(ctx, store.Webhook{ID: "nope"})
	is(t, err, store.ErrNotFound)

	now := time.Now()
	d1 := must(s.CreateDelivery(ctx, store.Delivery{WebhookID: w.ID, EventID: ids[0], NextAttemptAt: now.Add(-time.Minute)}))
	must(s.CreateDelivery(ctx, store.Delivery{WebhookID: w.ID, EventID: ids[1], NextAttemptAt: now.Add(time.Hour)}))
	_, err = s.CreateDelivery(ctx, store.Delivery{WebhookID: "nope", EventID: ids[1]})
	is(t, err, store.ErrConflict)
	if due := must(s.ListDueDeliveries(ctx, now, 10)); len(due) != 1 || due[0].ID != d1.ID || due[0].Status != store.DeliveryPending {
		t.Fatalf("due = %+v", due)
	}
	d1.Status, d1.Attempts, d1.LastStatus, d1.LastError = store.DeliveryDelivered, 1, 204, ""
	if err := s.UpdateDelivery(ctx, d1); err != nil {
		t.Fatal(err)
	}
	if due := must(s.ListDueDeliveries(ctx, now, 10)); len(due) != 0 {
		t.Fatalf("delivered still due: %+v", due)
	}
	if all := must(s.ListDeliveries(ctx, w.ID, "", 10)); len(all) != 2 || all[0].Status != store.DeliveryDelivered || all[0].LastStatus != 204 {
		t.Fatalf("deliveries = %+v", all)
	}
	is(t, s.UpdateDelivery(ctx, store.Delivery{ID: "nope"}), store.ErrNotFound)

	if err := s.DeleteWebhook(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	is(t, s.DeleteWebhook(ctx, w.ID), store.ErrNotFound)
	if all := must(s.ListDeliveries(ctx, w.ID, "", 10)); len(all) != 0 {
		t.Fatalf("deliveries survive webhook deletion: %+v", all)
	}
}
