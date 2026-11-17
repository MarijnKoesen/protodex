// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/b5"
	"github.com/marijnkoesen/protodex/internal/blob"
	"github.com/marijnkoesen/protodex/internal/owner"
)

type pushEnv struct {
	authEnv
	up   modulev1connect.UploadServiceClient
	cc   modulev1connect.CommitServiceClient
	lc   modulev1connect.LabelServiceClient
	mods modulev1connect.ModuleServiceClient
}

func newPushEnv(t *testing.T) pushEnv {
	t.Helper()
	e := newAuthEnv(t)
	mustOwner(t, e.st, owner.Organization, "acme")
	pe := pushEnv{authEnv: e,
		up:   modulev1connect.NewUploadServiceClient(http.DefaultClient, e.url, bearer(e.secret)),
		cc:   modulev1connect.NewCommitServiceClient(http.DefaultClient, e.url, bearer(e.secret)),
		lc:   modulev1connect.NewLabelServiceClient(http.DefaultClient, e.url, bearer(e.secret)),
		mods: modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(e.secret))}
	return pe
}

func (e pushEnv) module(t *testing.T, name string) {
	t.Helper()
	createModule(t, e.mods, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: name,
		Visibility: modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC})
}

func protoFiles(kv ...string) []*modulev1.File {
	var fs []*modulev1.File
	for pair := range slices.Chunk(kv, 2) {
		fs = append(fs, &modulev1.File{Path: pair[0], Content: []byte(pair[1])})
	}
	return fs
}

func upload(c modulev1connect.UploadServiceClient, mod string, files []*modulev1.File, deps []string, labels ...string) (*modulev1.Commit, error) {
	content := &modulev1.UploadRequest_Content{ModuleRef: modName("acme", mod), Files: files}
	for _, l := range labels {
		content.ScopedLabelRefs = append(content.ScopedLabelRefs, &modulev1.ScopedLabelRef{Value: &modulev1.ScopedLabelRef_Name{Name: l}})
	}
	resp, err := c.Upload(context.Background(), connect.NewRequest(&modulev1.UploadRequest{
		Contents: []*modulev1.UploadRequest_Content{content}, DepCommitIds: deps}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCommits()[0], nil
}

func TestUploadStoresCommit(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	e.module(t, "weather")

	files := protoFiles("a/v1/a.proto", `syntax = "proto3";`, "LICENSE", "Apache", "buf.yaml", "version: v2", "notes.txt", "ignored")
	c, err := upload(e.up, "weather", files, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The digest matches the independent computation over module files only.
	want, err := b5.Digest([]b5.File{{Path: "LICENSE", Content: []byte("Apache")}, {Path: "a/v1/a.proto", Content: []byte(`syntax = "proto3";`)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, _ := b5.Bytes(want)
	if c.GetDigest().GetType() != modulev1.DigestType_DIGEST_TYPE_B5 || !bytes.Equal(c.GetDigest().GetValue(), wantBytes) {
		t.Fatalf("digest = %v, want %s", c.GetDigest(), want)
	}
	if len(c.GetId()) != 32 {
		t.Fatalf("commit id %q", c.GetId())
	}

	// Metadata, manifest and blobs are stored; non-module files are dropped.
	m, err := e.st.GetModuleByID(ctx, c.GetModuleId())
	if err != nil {
		t.Fatal(err)
	}
	man, err := e.st.GetManifest(ctx, c.GetId())
	if err != nil || len(man) != 2 || man[0].Path != "LICENSE" || man[1].Path != "a/v1/a.proto" {
		t.Fatalf("manifest = %+v, %v", man, err)
	}
	d, err := blob.ParseDigest(man[0].BlobDigest)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.blobs.Get(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r); string(b) != "Apache" {
		t.Fatalf("blob = %q", b)
	}
	_ = r.Close()
	// The default label now exists and points at the commit.
	l, err := e.st.GetLabel(ctx, m.ID, "main")
	if err != nil || l.CommitID != c.GetId() {
		t.Fatalf("label = %+v, %v", l, err)
	}

	// CommitService reads it back by id, label and module.
	for _, ref := range []*modulev1.ResourceRef{
		{Value: &modulev1.ResourceRef_Id{Id: c.GetId()}},
		{Value: &modulev1.ResourceRef_Name_{Name: &modulev1.ResourceRef_Name{Owner: "acme", Module: "weather", Child: &modulev1.ResourceRef_Name_Ref{Ref: "main"}}}},
		{Value: &modulev1.ResourceRef_Name_{Name: &modulev1.ResourceRef_Name{Owner: "acme", Module: "weather", Child: &modulev1.ResourceRef_Name_Ref{Ref: c.GetId()}}}},
		{Value: &modulev1.ResourceRef_Name_{Name: &modulev1.ResourceRef_Name{Owner: "acme", Module: "weather"}}},
	} {
		resp, err := e.cc.GetCommits(ctx, connect.NewRequest(&modulev1.GetCommitsRequest{ResourceRefs: []*modulev1.ResourceRef{ref}}))
		if err != nil || resp.Msg.GetCommits()[0].GetId() != c.GetId() || !bytes.Equal(resp.Msg.GetCommits()[0].GetDigest().GetValue(), wantBytes) {
			t.Fatalf("GetCommits(%v) = %v, %v", ref, resp, err)
		}
	}
	_, err = e.cc.GetCommits(ctx, connect.NewRequest(&modulev1.GetCommitsRequest{ResourceRefs: []*modulev1.ResourceRef{
		{Value: &modulev1.ResourceRef_Id{Id: strings.Repeat("a", 32)}}}}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestUploadIdempotentAndHistory(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	e.module(t, "weather")
	a := protoFiles("a.proto", "// a")
	b := protoFiles("a.proto", "// b")

	c1, err := upload(e.up, "weather", a, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := upload(e.up, "weather", a, nil)
	if err != nil || again.GetId() != c1.GetId() {
		t.Fatalf("identical re-push = %v, %v; want commit %s", again, err, c1.GetId())
	}
	// A different label on identical content reuses the commit.
	other, err := upload(e.up, "weather", a, nil, "feature/x")
	if err != nil || other.GetId() != c1.GetId() {
		t.Fatalf("re-push on label = %v, %v", other, err)
	}
	c2, err := upload(e.up, "weather", b, nil)
	if err != nil || c2.GetId() == c1.GetId() {
		t.Fatalf("changed push = %v, %v", c2, err)
	}
	// Reverting to old content is a new commit (labels never move backwards).
	c3, err := upload(e.up, "weather", a, nil)
	if err != nil || c3.GetId() == c1.GetId() || c3.GetId() == c2.GetId() || !bytes.Equal(c3.GetDigest().GetValue(), c1.GetDigest().GetValue()) {
		t.Fatalf("revert push = %v, %v", c3, err)
	}

	resp, err := e.cc.ListCommits(ctx, connect.NewRequest(&modulev1.ListCommitsRequest{ResourceRef: moduleResource("acme", "weather")}))
	if err != nil || len(resp.Msg.GetCommits()) != 3 || resp.Msg.GetCommits()[0].GetId() != c3.GetId() {
		t.Fatalf("ListCommits = %v, %v", resp, err)
	}
	asc, err := e.cc.ListCommits(ctx, connect.NewRequest(&modulev1.ListCommitsRequest{ResourceRef: moduleResource("acme", "weather"),
		Order: modulev1.ListCommitsRequest_ORDER_CREATE_TIME_ASC, PageSize: 2}))
	if err != nil || len(asc.Msg.GetCommits()) != 2 || asc.Msg.GetCommits()[0].GetId() != c1.GetId() || asc.Msg.GetNextPageToken() == "" {
		t.Fatalf("ListCommits asc = %v, %v", asc, err)
	}
	// A label lists only the commit it points at; id_query matches substrings.
	hist, err := e.cc.ListCommits(ctx, connect.NewRequest(&modulev1.ListCommitsRequest{ResourceRef: &modulev1.ResourceRef{
		Value: &modulev1.ResourceRef_Name_{Name: &modulev1.ResourceRef_Name{Owner: "acme", Module: "weather", Child: &modulev1.ResourceRef_Name_LabelName{LabelName: "main"}}}}}))
	if err != nil || len(hist.Msg.GetCommits()) != 1 || hist.Msg.GetCommits()[0].GetId() != c3.GetId() {
		t.Fatalf("label listing = %v, %v", hist, err)
	}
	mid := strings.ToUpper(c2.GetId()[10:20])
	q, err := e.cc.ListCommits(ctx, connect.NewRequest(&modulev1.ListCommitsRequest{ResourceRef: moduleResource("acme", "weather"), IdQuery: mid}))
	if err != nil || !slices.ContainsFunc(q.Msg.GetCommits(), func(c *modulev1.Commit) bool { return c.GetId() == c2.GetId() }) {
		t.Fatalf("id_query %q = %v, %v", mid, q, err)
	}
}

func TestUploadDependencies(t *testing.T) {
	e := newPushEnv(t)
	e.module(t, "base")
	e.module(t, "app")
	base, err := upload(e.up, "base", protoFiles("b.proto", "// b"), nil)
	if err != nil {
		t.Fatal(err)
	}
	files := protoFiles("a.proto", "// a")
	app, err := upload(e.up, "app", files, []string{base.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	// Dependencies are part of the digest.
	nodep, err := upload(e.up, "app", files, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(app.GetDigest().GetValue(), nodep.GetDigest().GetValue()) {
		t.Fatal("digest ignores dependencies")
	}
	want, _ := b5.Digest([]b5.File{{Path: "a.proto", Content: []byte("// a")}}, []string{"b5:" + hexOf(base.GetDigest().GetValue())})
	if got := "b5:" + hexOf(app.GetDigest().GetValue()); got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
	deps, err := e.st.ListCommitDeps(context.Background(), app.GetId())
	if err != nil || len(deps) != 1 || deps[0].DepOwner != "acme" || deps[0].DepModule != "base" || deps[0].DepCommitID != base.GetId() {
		t.Fatalf("deps = %+v, %v", deps, err)
	}

	// Unknown dependency, and a dependency on itself.
	_, err = upload(e.up, "app", files, []string{strings.Repeat("b", 32)})
	wantCode(t, err, connect.CodeNotFound)
	_, err = upload(e.up, "app", files, []string{app.GetId()})
	wantCode(t, err, connect.CodeInvalidArgument)
	if n := len(mustCommits(t, e, "app")); n != 2 {
		t.Fatalf("failed pushes left %d commits, want 2", n)
	}
}

func hexOf(b []byte) string {
	const hexd = "0123456789abcdef"
	var sb strings.Builder
	for _, x := range b {
		sb.WriteByte(hexd[x>>4])
		sb.WriteByte(hexd[x&15])
	}
	return sb.String()
}

func mustCommits(t *testing.T, e pushEnv, mod string) []*modulev1.Commit {
	t.Helper()
	resp, err := e.cc.ListCommits(context.Background(), connect.NewRequest(&modulev1.ListCommitsRequest{ResourceRef: moduleResource("acme", mod)}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetCommits()
}

func TestUploadRejects(t *testing.T) {
	e := newPushEnv(t)
	e.module(t, "weather")
	_, bobSecret := userToken(t, e.st, "bob")
	bob := modulev1connect.NewUploadServiceClient(http.DefaultClient, e.url, bearer(bobSecret))
	anon := modulev1connect.NewUploadServiceClient(http.DefaultClient, e.url)
	mirror := mustOwner(t, e.st, owner.Organization, "mirrored")
	_ = mirror

	ok := protoFiles("a.proto", "// a")
	_, err := upload(anon, "weather", ok, nil)
	wantCode(t, err, connect.CodeUnauthenticated)
	// Users may write organizations, but not another user's namespace.
	if _, err := upload(bob, "weather", ok, nil); err != nil {
		t.Fatalf("org push by user: %v", err)
	}
	_, err = upload(e.up, "nope", ok, nil)
	wantCode(t, err, connect.CodeNotFound)

	tooMany := protoFiles("1.proto", "", "2.proto", "", "3.proto", "", "4.proto", "", "5.proto", "", "6.proto", "")
	_, err = upload(e.up, "weather", tooMany, nil)
	wantCode(t, err, connect.CodeInvalidArgument)
	big := protoFiles("big.proto", strings.Repeat("x", 2<<10))
	_, err = upload(e.up, "weather", big, nil)
	wantCode(t, err, connect.CodeInvalidArgument)
	// Each file is within the per-file limit, but together they exceed the push quota.
	_, err = upload(e.up, "weather", protoFiles("a.proto", strings.Repeat("x", 1000), "b.proto", strings.Repeat("y", 1000)), nil)
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = upload(e.up, "weather", protoFiles("a.proto", "1", "a.proto", "2"), nil)
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = upload(e.up, "weather", protoFiles("buf.yaml", "version: v2"), nil)
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = upload(e.up, "weather", ok, nil, "bad label")
	wantCode(t, err, connect.CodeInvalidArgument)
	if n := len(mustCommits(t, e, "weather")); n != 1 {
		t.Fatalf("commits = %d, want 1 (only the valid push)", n)
	}
}

func TestUploadConcurrentPushesToOneLabel(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	e.module(t, "weather")

	const n = 12
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := upload(e.up, "weather", protoFiles("a.proto", "// "+strconv.Itoa(i)), nil)
			errs[i] = err
			if err == nil {
				ids[i] = c.GetId()
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("push %d: %v", i, errs[i])
		}
		seen[ids[i]] = true
	}
	if len(seen) != n {
		t.Fatalf("%d distinct commits, want %d", len(seen), n)
	}
	cs := mustCommits(t, e, "weather")
	if len(cs) != n {
		t.Fatalf("stored %d commits, want %d", len(cs), n)
	}
	// The label ends on the newest commit, whatever the interleaving.
	l, err := e.lc.GetLabels(ctx, connect.NewRequest(&modulev1.GetLabelsRequest{LabelRefs: []*modulev1.LabelRef{labelName("acme", "weather", "main")}}))
	if err != nil || l.Msg.GetLabels()[0].GetCommitId() != cs[0].GetId() {
		t.Fatalf("label = %v, %v; newest commit %s", l, err, cs[0].GetId())
	}
}
