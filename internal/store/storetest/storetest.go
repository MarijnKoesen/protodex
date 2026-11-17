// SPDX-License-Identifier: Apache-2.0

// Package storetest is the backend-agnostic conformance suite for store.Store.
package storetest

import (
	"context"
	"errors"
	"fmt"
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
