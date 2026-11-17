// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/descriptorpb"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
)

// readClients are the read services, with the admin token or anonymous.
type readClients struct {
	dl  modulev1connect.DownloadServiceClient
	rs  modulev1connect.ResourceServiceClient
	gs  modulev1connect.GraphServiceClient
	fds modulev1connect.FileDescriptorSetServiceClient
}

func newReadClients(url string, opts ...connect.ClientOption) readClients {
	return readClients{
		dl:  modulev1connect.NewDownloadServiceClient(http.DefaultClient, url, opts...),
		rs:  modulev1connect.NewResourceServiceClient(http.DefaultClient, url, opts...),
		gs:  modulev1connect.NewGraphServiceClient(http.DefaultClient, url, opts...),
		fds: modulev1connect.NewFileDescriptorSetServiceClient(http.DefaultClient, url, opts...),
	}
}

func refName(o, m, ref string) *modulev1.ResourceRef {
	return &modulev1.ResourceRef{Value: &modulev1.ResourceRef_Name_{Name: &modulev1.ResourceRef_Name{Owner: o, Module: m, Child: &modulev1.ResourceRef_Name_Ref{Ref: ref}}}}
}

func refLabel(o, m, label string) *modulev1.ResourceRef {
	return &modulev1.ResourceRef{Value: &modulev1.ResourceRef_Name_{Name: &modulev1.ResourceRef_Name{Owner: o, Module: m, Child: &modulev1.ResourceRef_Name_LabelName{LabelName: label}}}}
}

func refID(id string) *modulev1.ResourceRef {
	return &modulev1.ResourceRef{Value: &modulev1.ResourceRef_Id{Id: id}}
}

func download(c modulev1connect.DownloadServiceClient, vs ...*modulev1.DownloadRequest_Value) (*modulev1.DownloadResponse, error) {
	resp, err := c.Download(context.Background(), connect.NewRequest(&modulev1.DownloadRequest{Values: vs}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func filePaths(fs []*modulev1.File) (out []string) {
	for _, f := range fs {
		out = append(out, f.GetPath())
	}
	return out
}

const (
	baseProto = `syntax = "proto3";
package acme.base.v1;
// A place.
message Place { string name = 1; }
`
	appProto = `syntax = "proto3";
package acme.app.v1;
import "base/v1/base.proto";
import "google/protobuf/timestamp.proto";
message Visit {
  acme.base.v1.Place place = 1;
  google.protobuf.Timestamp at = 2;
}
`
)

func TestDownload(t *testing.T) {
	e := newPushEnv(t)
	rc := newReadClients(e.url, bearer(e.secret))
	e.module(t, "weather")
	old, err := upload(e.up, "weather", protoFiles("a/v1/a.proto", "// old"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := upload(e.up, "weather", protoFiles("a/v1/a.proto", "// a", "a/v1/b.proto", "// b", "c/c.proto", "// c", "LICENSE", "Apache", "README.md", "# W"), nil, "main", "v1")
	if err != nil {
		t.Fatal(err)
	}

	// A module, its labels and the commit all resolve to the same content.
	all := []string{"LICENSE", "README.md", "a/v1/a.proto", "a/v1/b.proto", "c/c.proto"}
	for _, ref := range []*modulev1.ResourceRef{
		moduleResource("acme", "weather"),
		refLabel("acme", "weather", "v1"),
		refName("acme", "weather", "main"),
		refName("acme", "weather", c.GetId()),
		refID(c.GetId()),
	} {
		resp, err := download(rc.dl, &modulev1.DownloadRequest_Value{ResourceRef: ref})
		if err != nil {
			t.Fatalf("Download(%v): %v", ref, err)
		}
		got := resp.GetContents()[0]
		if got.GetCommit().GetId() != c.GetId() || !slices.Equal(filePaths(got.GetFiles()), all) {
			t.Fatalf("Download(%v) = %s %v", ref, got.GetCommit().GetId(), filePaths(got.GetFiles()))
		}
		if string(got.GetFiles()[2].GetContent()) != "// a" {
			t.Fatalf("content = %q", got.GetFiles()[2].GetContent())
		}
	}

	// An older commit by id, in the same batch.
	resp, err := download(rc.dl,
		&modulev1.DownloadRequest_Value{ResourceRef: refName("acme", "weather", old.GetId())},
		&modulev1.DownloadRequest_Value{ResourceRef: refID(c.GetId())})
	if err != nil || len(resp.GetContents()) != 2 || string(resp.GetContents()[0].GetFiles()[0].GetContent()) != "// old" {
		t.Fatalf("batch = %v, %v", resp, err)
	}

	// File types and paths (directories select the files below them).
	for _, tc := range []struct {
		types []modulev1.FileType
		paths []string
		want  []string
	}{
		{types: []modulev1.FileType{modulev1.FileType_FILE_TYPE_PROTO}, want: []string{"a/v1/a.proto", "a/v1/b.proto", "c/c.proto"}},
		{types: []modulev1.FileType{modulev1.FileType_FILE_TYPE_DOC, modulev1.FileType_FILE_TYPE_LICENSE}, want: []string{"LICENSE", "README.md"}},
		{paths: []string{"a/v1"}, want: []string{"a/v1/a.proto", "a/v1/b.proto"}},
		{paths: []string{"a/v1/b.proto", "LICENSE"}, want: []string{"LICENSE", "a/v1/b.proto"}},
		{types: []modulev1.FileType{modulev1.FileType_FILE_TYPE_PROTO}, paths: []string{"a"}, want: []string{"a/v1/a.proto", "a/v1/b.proto"}},
	} {
		resp, err := download(rc.dl, &modulev1.DownloadRequest_Value{ResourceRef: moduleResource("acme", "weather"), FileTypes: tc.types, Paths: tc.paths})
		if err != nil {
			t.Fatalf("types %v paths %v: %v", tc.types, tc.paths, err)
		}
		if got := filePaths(resp.GetContents()[0].GetFiles()); !slices.Equal(got, tc.want) {
			t.Fatalf("types %v paths %v = %v, want %v", tc.types, tc.paths, got, tc.want)
		}
	}

	// A path that matches nothing ("a/v" is not a directory; LICENSE is not
	// a proto) fails unless allowed.
	for _, v := range []*modulev1.DownloadRequest_Value{
		{ResourceRef: moduleResource("acme", "weather"), Paths: []string{"a/v"}},
		{ResourceRef: moduleResource("acme", "weather"), Paths: []string{"LICENSE"}, FileTypes: []modulev1.FileType{modulev1.FileType_FILE_TYPE_PROTO}},
	} {
		_, err = download(rc.dl, v)
		wantCode(t, err, connect.CodeNotFound)
		v.PathsAllowNotExist = true
		resp, err = download(rc.dl, v)
		if err != nil || len(resp.GetContents()[0].GetFiles()) != 0 {
			t.Fatalf("allowed missing path = %v, %v", resp, err)
		}
	}

	// Unknown module, label and commit; a module with no commits.
	e.module(t, "empty")
	for _, ref := range []*modulev1.ResourceRef{
		moduleResource("acme", "nope"),
		refLabel("acme", "weather", "nope"),
		refName("acme", "weather", "0123456789abcdef0123456789abcdef"),
		moduleResource("acme", "empty"),
	} {
		_, err = download(rc.dl, &modulev1.DownloadRequest_Value{ResourceRef: ref})
		wantCode(t, err, connect.CodeNotFound)
	}
}

// TestReadsHidePrivateModules covers the read-access hook: a private module
// is NotFound for callers that may not read it, on every read service.
func TestReadsHidePrivateModules(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	createModule(t, e.mods, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("admin"), Name: "secret"})
	resp, err := e.up.Upload(ctx, connect.NewRequest(&modulev1.UploadRequest{Contents: []*modulev1.UploadRequest_Content{
		{ModuleRef: modName("admin", "secret"), Files: protoFiles("s.proto", `syntax = "proto3";`)}}}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetCommits()[0].GetId()

	_, other := userToken(t, e.st, "mallory")
	for name, rc := range map[string]readClients{
		"anonymous":  newReadClients(e.url),
		"other user": newReadClients(e.url, bearer(other)),
	} {
		for _, ref := range []*modulev1.ResourceRef{moduleResource("admin", "secret"), refID(id), refLabel("admin", "secret", "main")} {
			_, err := download(rc.dl, &modulev1.DownloadRequest_Value{ResourceRef: ref})
			wantCode(t, err, connect.CodeNotFound)
			_, err = rc.rs.GetResources(ctx, connect.NewRequest(&modulev1.GetResourcesRequest{ResourceRefs: []*modulev1.ResourceRef{ref}}))
			wantCode(t, err, connect.CodeNotFound)
			_, err = rc.gs.GetGraph(ctx, connect.NewRequest(&modulev1.GetGraphRequest{ResourceRefs: []*modulev1.ResourceRef{ref}}))
			wantCode(t, err, connect.CodeNotFound)
			_, err = rc.fds.GetFileDescriptorSet(ctx, connect.NewRequest(&modulev1.GetFileDescriptorSetRequest{ResourceRef: ref}))
			wantCode(t, err, connect.CodeNotFound)
		}
		t.Logf("%s: private module hidden", name)
	}
	// The owner reads it.
	if _, err := download(newReadClients(e.url, bearer(e.secret)).dl, &modulev1.DownloadRequest_Value{ResourceRef: refID(id)}); err != nil {
		t.Fatal(err)
	}
	// Public modules are readable anonymously.
	e.module(t, "open")
	if _, err := upload(e.up, "open", protoFiles("o.proto", "// o"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := download(newReadClients(e.url).dl, &modulev1.DownloadRequest_Value{ResourceRef: moduleResource("acme", "open")}); err != nil {
		t.Fatal(err)
	}
}

func TestGetResources(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	rc := newReadClients(e.url, bearer(e.secret))
	e.module(t, "weather")
	c, err := upload(e.up, "weather", protoFiles("a.proto", "// a"), nil, "main", "v1")
	if err != nil {
		t.Fatal(err)
	}
	l, err := e.lc.GetLabels(ctx, connect.NewRequest(&modulev1.GetLabelsRequest{LabelRefs: []*modulev1.LabelRef{labelName("acme", "weather", "v1")}}))
	if err != nil {
		t.Fatal(err)
	}
	labelID := l.Msg.GetLabels()[0].GetId()
	e.module(t, "empty")

	resp, err := rc.rs.GetResources(ctx, connect.NewRequest(&modulev1.GetResourcesRequest{ResourceRefs: []*modulev1.ResourceRef{
		moduleResource("acme", "weather"),
		refName("acme", "weather", "v1"),
		refLabel("acme", "weather", "main"),
		refName("acme", "weather", c.GetId()),
		refID(c.GetId()),
		refID(labelID),
		refID(c.GetModuleId()),
		moduleResource("acme", "empty"),
	}}))
	if err != nil {
		t.Fatal(err)
	}
	rs := resp.Msg.GetResources()
	if len(rs) != 8 {
		t.Fatalf("resources = %v", rs)
	}
	if m := rs[0].GetModule(); m.GetName() != "weather" {
		t.Fatalf("module ref = %v", rs[0])
	}
	if l := rs[1].GetLabel(); l.GetName() != "v1" || l.GetCommitId() != c.GetId() {
		t.Fatalf("name:ref label = %v", rs[1])
	}
	if l := rs[2].GetLabel(); l.GetName() != "main" {
		t.Fatalf("label_name = %v", rs[2])
	}
	for _, i := range []int{3, 4} {
		if cm := rs[i].GetCommit(); cm.GetId() != c.GetId() || cm.GetDigest() == nil {
			t.Fatalf("commit ref %d = %v", i, rs[i])
		}
	}
	if l := rs[5].GetLabel(); l.GetId() != labelID {
		t.Fatalf("label id = %v", rs[5])
	}
	if m := rs[6].GetModule(); m.GetId() != c.GetModuleId() {
		t.Fatalf("module id = %v", rs[6])
	}
	if m := rs[7].GetModule(); m.GetName() != "empty" {
		t.Fatalf("module without commits = %v", rs[7])
	}

	_, err = rc.rs.GetResources(ctx, connect.NewRequest(&modulev1.GetResourcesRequest{ResourceRefs: []*modulev1.ResourceRef{refName("acme", "weather", "nope")}}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestGetGraph(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	rc := newReadClients(e.url, bearer(e.secret))
	e.module(t, "base")
	e.module(t, "mid")
	e.module(t, "app")
	base, err := upload(e.up, "base", protoFiles("b.proto", "// b"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mid, err := upload(e.up, "mid", protoFiles("m.proto", "// m"), []string{base.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	app, err := upload(e.up, "app", protoFiles("a.proto", "// a"), []string{mid.GetId(), base.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rc.gs.GetGraph(ctx, connect.NewRequest(&modulev1.GetGraphRequest{ResourceRefs: []*modulev1.ResourceRef{moduleResource("acme", "app")}}))
	if err != nil {
		t.Fatal(err)
	}
	g := resp.Msg.GetGraph()
	var ids []string
	for _, c := range g.GetCommits() {
		ids = append(ids, c.GetId())
	}
	if len(ids) != 3 || ids[0] != app.GetId() || !slices.Contains(ids, mid.GetId()) || !slices.Contains(ids, base.GetId()) {
		t.Fatalf("commits = %v", ids)
	}
	var edges []string
	for _, ed := range g.GetEdges() {
		edges = append(edges, ed.GetFromNode().GetCommitId()+">"+ed.GetToNode().GetCommitId())
	}
	slices.Sort(edges)
	want := []string{app.GetId() + ">" + base.GetId(), app.GetId() + ">" + mid.GetId(), mid.GetId() + ">" + base.GetId()}
	slices.Sort(want)
	if !slices.Equal(edges, want) {
		t.Fatalf("edges = %v, want %v", edges, want)
	}
}

func TestGetGraphDiamondAndConflict(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	rc := newReadClients(e.url, bearer(e.secret))
	for _, m := range []string{"base", "left", "right", "app"} {
		e.module(t, m)
	}
	base1, err := upload(e.up, "base", protoFiles("b.proto", "// b1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	base2, err := upload(e.up, "base", protoFiles("b.proto", "// b2"), nil)
	if err != nil {
		t.Fatal(err)
	}
	left, err := upload(e.up, "left", protoFiles("l.proto", "// l"), []string{base1.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	right, err := upload(e.up, "right", protoFiles("r.proto", "// r"), []string{base2.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	app, err := upload(e.up, "app", protoFiles("a.proto", "// a"), []string{left.GetId(), right.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rc.gs.GetGraph(ctx, connect.NewRequest(&modulev1.GetGraphRequest{ResourceRefs: []*modulev1.ResourceRef{refID(app.GetId())}}))
	if err != nil {
		t.Fatal(err)
	}
	g := resp.Msg.GetGraph()
	pos := map[string]int{}
	for i, c := range g.GetCommits() {
		pos[c.GetId()] = i
	}
	if len(pos) != 5 || len(g.GetCommits()) != 5 {
		t.Fatalf("want 5 unique commits (both base versions), got %d", len(g.GetCommits()))
	}
	if len(g.GetEdges()) != 4 {
		t.Fatalf("edges = %v", g.GetEdges())
	}
	for _, ed := range g.GetEdges() {
		if pos[ed.GetFromNode().GetCommitId()] >= pos[ed.GetToNode().GetCommitId()] {
			t.Fatalf("edge %v not in topological order: %v", ed, pos)
		}
	}
}

func TestGetGraphSharedBaseOrdering(t *testing.T) {
	e := newPushEnv(t)
	rc := newReadClients(e.url, bearer(e.secret))
	for _, m := range []string{"base", "mid", "app"} {
		e.module(t, m)
	}
	base, err := upload(e.up, "base", protoFiles("b.proto", "// b"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mid, err := upload(e.up, "mid", protoFiles("m.proto", "// m"), []string{base.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	// app lists base before mid, so breadth-first discovery puts base before
	// mid; only the topological sort fixes that.
	app, err := upload(e.up, "app", protoFiles("a.proto", "// a"), []string{base.GetId(), mid.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rc.gs.GetGraph(context.Background(), connect.NewRequest(&modulev1.GetGraphRequest{ResourceRefs: []*modulev1.ResourceRef{refID(app.GetId())}}))
	if err != nil {
		t.Fatal(err)
	}
	g := resp.Msg.GetGraph()
	var ids []string
	for _, c := range g.GetCommits() {
		ids = append(ids, c.GetId())
	}
	if want := []string{app.GetId(), mid.GetId(), base.GetId()}; !slices.Equal(ids, want) {
		t.Fatalf("commits = %v, want %v", ids, want)
	}
	into := 0
	for _, ed := range g.GetEdges() {
		if ed.GetToNode().GetCommitId() == base.GetId() {
			into++
		}
	}
	if into != 2 || len(g.GetEdges()) != 3 {
		t.Fatalf("edges = %v", g.GetEdges())
	}
}

func TestGetFileDescriptorSet(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	rc := newReadClients(e.url, bearer(e.secret))
	e.module(t, "base")
	e.module(t, "app")
	base, err := upload(e.up, "base", protoFiles("base/v1/base.proto", baseProto), nil)
	if err != nil {
		t.Fatal(err)
	}
	app, err := upload(e.up, "app", protoFiles("app/v1/app.proto", appProto, "LICENSE", "x"), []string{base.GetId()}, "main", "v1")
	if err != nil {
		t.Fatal(err)
	}
	get := func(req *modulev1.GetFileDescriptorSetRequest) *modulev1.GetFileDescriptorSetResponse {
		t.Helper()
		resp, err := rc.fds.GetFileDescriptorSet(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("GetFileDescriptorSet(%v): %v", req, err)
		}
		return resp.Msg
	}
	names := func(set *descriptorpb.FileDescriptorSet) (out []string) {
		for _, f := range set.GetFile() {
			out = append(out, f.GetName())
		}
		return out
	}

	// Imports first; the same set for the module, a label and the commit.
	want := []string{"base/v1/base.proto", "google/protobuf/timestamp.proto", "app/v1/app.proto"}
	for _, ref := range []*modulev1.ResourceRef{moduleResource("acme", "app"), refName("acme", "app", "v1"), refID(app.GetId())} {
		for range 2 { // the second call is served from the cache
			resp := get(&modulev1.GetFileDescriptorSetRequest{ResourceRef: ref})
			if got := names(resp.GetFileDescriptorSet()); !slices.Equal(got, want) {
				t.Fatalf("files = %v, want %v", got, want)
			}
			if resp.GetCommit().GetId() != app.GetId() {
				t.Fatalf("commit = %v", resp.GetCommit())
			}
			for _, f := range resp.GetFileDescriptorSet().GetFile() {
				if f.GetSourceCodeInfo() != nil {
					t.Fatalf("%s has source info without include_source_code_info", f.GetName())
				}
			}
		}
	}

	resp := get(&modulev1.GetFileDescriptorSetRequest{ResourceRef: moduleResource("acme", "app"), ExcludeImports: true, IncludeSourceCodeInfo: true})
	if got := names(resp.GetFileDescriptorSet()); !slices.Equal(got, []string{"app/v1/app.proto"}) {
		t.Fatalf("exclude_imports files = %v", got)
	}
	if resp.GetFileDescriptorSet().GetFile()[0].GetSourceCodeInfo() == nil {
		t.Fatal("include_source_code_info: no source info")
	}
	fd := resp.GetFileDescriptorSet().GetFile()[0]
	if fd.GetMessageType()[0].GetField()[0].GetTypeName() != ".acme.base.v1.Place" {
		t.Fatalf("app.proto = %v", fd)
	}

	// Type filters are not supported yet.
	_, err = rc.fds.GetFileDescriptorSet(ctx, connect.NewRequest(&modulev1.GetFileDescriptorSetRequest{ResourceRef: refID(app.GetId()), IncludeTypes: []string{"acme.app.v1.Visit"}}))
	wantCode(t, err, connect.CodeUnimplemented)

	// A module that does not compile.
	e.module(t, "broken")
	if _, err := upload(e.up, "broken", protoFiles("x.proto", `syntax = "proto3"; message X { Missing m = 1; }`), nil); err != nil {
		t.Fatal(err)
	}
	_, err = rc.fds.GetFileDescriptorSet(ctx, connect.NewRequest(&modulev1.GetFileDescriptorSetRequest{ResourceRef: moduleResource("acme", "broken")}))
	wantCode(t, err, connect.CodeFailedPrecondition)
}
