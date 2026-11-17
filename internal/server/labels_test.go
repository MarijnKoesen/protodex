// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/store"
)

func labelName(o, m, l string) *modulev1.LabelRef {
	return &modulev1.LabelRef{Value: &modulev1.LabelRef_Name_{Name: &modulev1.LabelRef_Name{Owner: o, Module: m, Label: l}}}
}

func moduleResource(o, m string) *modulev1.ResourceRef {
	return &modulev1.ResourceRef{Value: &modulev1.ResourceRef_Name_{Name: &modulev1.ResourceRef_Name{Owner: o, Module: m}}}
}

func setLabel(t *testing.T, c modulev1connect.LabelServiceClient, ref *modulev1.LabelRef, commit string) (*modulev1.Label, error) {
	t.Helper()
	resp, err := c.CreateOrUpdateLabels(context.Background(), connect.NewRequest(&modulev1.CreateOrUpdateLabelsRequest{
		Values: []*modulev1.CreateOrUpdateLabelsRequest_Value{{LabelRef: ref, CommitId: commit}}}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetLabels()[0], nil
}

func labelNames(ls []*modulev1.Label) (out []string) {
	for _, l := range ls {
		out = append(out, l.GetName())
	}
	return out
}

func TestLabelService(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	mustOwner(t, e.st, owner.Organization, "acme")
	admin, err := e.st.GetOwner(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, bobSecret := userToken(t, e.st, "bob")
	mods := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(e.secret))
	lc := modulev1connect.NewLabelServiceClient(http.DefaultClient, e.url, bearer(e.secret))
	anon := modulev1connect.NewLabelServiceClient(http.DefaultClient, e.url)
	bob := modulev1connect.NewLabelServiceClient(http.DefaultClient, e.url, bearer(bobSecret))

	m := createModule(t, mods, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("acme"), Name: "weather",
		Visibility: modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC})
	other := createModule(t, mods, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("admin"), Name: "private"})
	base := time.Now().Add(-time.Hour)
	commit := func(moduleID string, at time.Duration) store.Commit {
		c, err := e.st.CreateCommit(ctx, store.Commit{ModuleID: moduleID, Digest: "b5:00", CreatedAt: base.Add(at)})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c1, c2 := commit(m.GetId(), 0), commit(m.GetId(), time.Minute)
	foreign := commit(other.GetId(), 0)

	// A new module has no labels, not even the default one.
	ll, err := lc.ListLabels(ctx, connect.NewRequest(&modulev1.ListLabelsRequest{ResourceRef: moduleResource("acme", "weather")}))
	if err != nil || len(ll.Msg.GetLabels()) != 0 {
		t.Fatalf("ListLabels on new module = %v, %v", ll, err)
	}

	// Create, then move forward; moving back is refused.
	main, err := setLabel(t, lc, labelName("acme", "weather", "main"), c1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(main.GetId()) != 32 || main.GetCommitId() != c1.ID || main.GetModuleId() != m.GetId() ||
		main.GetOwnerId() != m.GetOwnerId() || main.GetUpdatedByUserId() != admin.ID || main.GetArchiveTime() != nil ||
		main.GetCommitCheckState().GetStatus() != modulev1.CommitCheckStatus_COMMIT_CHECK_STATUS_DISABLED {
		t.Fatalf("created label = %v", main)
	}
	moved, err := setLabel(t, lc, &modulev1.LabelRef{Value: &modulev1.LabelRef_Id{Id: main.GetId()}}, c2.ID)
	if err != nil || moved.GetId() != main.GetId() || moved.GetCommitId() != c2.ID {
		t.Fatalf("moved label = %v, %v", moved, err)
	}
	_, err = setLabel(t, lc, labelName("acme", "weather", "main"), c1.ID)
	wantCode(t, err, connect.CodeFailedPrecondition)
	// Same commit again is a no-op.
	if again, err := setLabel(t, lc, labelName("acme", "weather", "main"), c2.ID); err != nil || !again.GetUpdateTime().AsTime().Equal(moved.GetUpdateTime().AsTime()) {
		t.Fatalf("idempotent set = %v, %v", again, err)
	}

	for _, tc := range []struct {
		c      modulev1connect.LabelServiceClient
		ref    *modulev1.LabelRef
		commit string
		want   connect.Code
	}{
		{anon, labelName("acme", "weather", "x"), c1.ID, connect.CodeUnauthenticated},
		{lc, labelName("acme", "weather", "x"), foreign.ID, connect.CodeNotFound}, // commit of another module
		{lc, labelName("acme", "weather", "x"), "0123456789abcdef0123456789abcdef", connect.CodeNotFound},
		{lc, labelName("acme", "nope", "x"), c1.ID, connect.CodeNotFound},
		{lc, labelName("acme", "weather", "a b"), c1.ID, connect.CodeInvalidArgument},
		{lc, &modulev1.LabelRef{Value: &modulev1.LabelRef_Id{Id: "0123456789abcdef0123456789abcdef"}}, c1.ID, connect.CodeNotFound},
		{bob, labelName("admin", "private", "x"), foreign.ID, connect.CodeNotFound}, // hidden module
	} {
		_, err := setLabel(t, tc.c, tc.ref, tc.commit)
		wantCode(t, err, tc.want)
	}

	if _, err := setLabel(t, lc, labelName("acme", "weather", "v1.0.0"), c1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := setLabel(t, bob, labelName("acme", "weather", "feature/x"), c2.ID); err != nil {
		t.Fatal(err)
	}

	// GetLabels by name and id, anonymously for a public module.
	gl, err := anon.GetLabels(ctx, connect.NewRequest(&modulev1.GetLabelsRequest{LabelRefs: []*modulev1.LabelRef{
		labelName("acme", "weather", "v1.0.0"), {Value: &modulev1.LabelRef_Id{Id: main.GetId()}},
	}}))
	if err != nil || !slices.Equal(labelNames(gl.Msg.GetLabels()), []string{"v1.0.0", "main"}) {
		t.Fatalf("GetLabels = %v, %v", gl, err)
	}
	_, err = anon.GetLabels(ctx, connect.NewRequest(&modulev1.GetLabelsRequest{LabelRefs: []*modulev1.LabelRef{labelName("acme", "weather", "nope")}}))
	wantCode(t, err, connect.CodeNotFound)

	list := func(req *modulev1.ListLabelsRequest) []string {
		t.Helper()
		resp, err := anon.ListLabels(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("ListLabels(%v): %v", req, err)
		}
		return labelNames(resp.Msg.GetLabels())
	}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: moduleResource("acme", "weather")}); !slices.Equal(got, []string{"feature/x", "v1.0.0", "main"}) {
		t.Fatalf("ListLabels = %v", got)
	}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: moduleResource("acme", "weather"), Order: modulev1.ListLabelsRequest_ORDER_CREATE_TIME_ASC, PageSize: 2}); !slices.Equal(got, []string{"main", "v1.0.0"}) {
		t.Fatalf("ListLabels asc page = %v", got)
	}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: moduleResource("acme", "weather"), NameQuery: "V1"}); !slices.Equal(got, []string{"v1.0.0"}) {
		t.Fatalf("ListLabels name query = %v", got)
	}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: moduleResource("acme", "weather"),
		CommitCheckStatuses: []modulev1.CommitCheckStatus{modulev1.CommitCheckStatus_COMMIT_CHECK_STATUS_PASSED}}); len(got) != 0 {
		t.Fatalf("ListLabels passed-only = %v", got)
	}
	// Resource refs: a commit (by name ref and by id) selects the labels on it.
	byRef := &modulev1.ResourceRef{Value: &modulev1.ResourceRef_Name_{Name: &modulev1.ResourceRef_Name{
		Owner: "acme", Module: "weather", Child: &modulev1.ResourceRef_Name_Ref{Ref: c2.ID}}}}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: byRef}); !slices.Equal(got, []string{"feature/x", "main"}) {
		t.Fatalf("labels on c2 = %v", got)
	}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: &modulev1.ResourceRef{Value: &modulev1.ResourceRef_Id{Id: c1.ID}}}); !slices.Equal(got, []string{"v1.0.0"}) {
		t.Fatalf("labels on c1 = %v", got)
	}
	byLabel := &modulev1.ResourceRef{Value: &modulev1.ResourceRef_Name_{Name: &modulev1.ResourceRef_Name{
		Owner: "acme", Module: "weather", Child: &modulev1.ResourceRef_Name_LabelName{LabelName: "main"}}}}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: byLabel}); !slices.Equal(got, []string{"main"}) {
		t.Fatalf("label by name = %v", got)
	}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: &modulev1.ResourceRef{Value: &modulev1.ResourceRef_Id{Id: m.GetId()}}}); len(got) != 3 {
		t.Fatalf("labels by module id = %v", got)
	}
	_, err = anon.ListLabels(ctx, connect.NewRequest(&modulev1.ListLabelsRequest{ResourceRef: moduleResource("admin", "private")}))
	wantCode(t, err, connect.CodeNotFound)

	// Archiving: the default label cannot be archived; others can.
	_, err = lc.ArchiveLabels(ctx, connect.NewRequest(&modulev1.ArchiveLabelsRequest{LabelRefs: []*modulev1.LabelRef{labelName("acme", "weather", "main")}}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = anon.ArchiveLabels(ctx, connect.NewRequest(&modulev1.ArchiveLabelsRequest{LabelRefs: []*modulev1.LabelRef{labelName("acme", "weather", "v1.0.0")}}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = lc.ArchiveLabels(ctx, connect.NewRequest(&modulev1.ArchiveLabelsRequest{LabelRefs: []*modulev1.LabelRef{labelName("acme", "weather", "nope")}}))
	wantCode(t, err, connect.CodeNotFound)
	for range 2 { // idempotent
		if _, err := lc.ArchiveLabels(ctx, connect.NewRequest(&modulev1.ArchiveLabelsRequest{LabelRefs: []*modulev1.LabelRef{labelName("acme", "weather", "v1.0.0")}})); err != nil {
			t.Fatal(err)
		}
	}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: moduleResource("acme", "weather")}); slices.Contains(got, "v1.0.0") {
		t.Fatalf("archived label listed by default: %v", got)
	}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: moduleResource("acme", "weather"), ArchiveFilter: modulev1.ListLabelsRequest_ARCHIVE_FILTER_ARCHIVED_ONLY}); !slices.Equal(got, []string{"v1.0.0"}) {
		t.Fatalf("archived only = %v", got)
	}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: moduleResource("acme", "weather"), ArchiveFilter: modulev1.ListLabelsRequest_ARCHIVE_FILTER_ALL}); len(got) != 3 {
		t.Fatalf("all = %v", got)
	}
	gl, err = anon.GetLabels(ctx, connect.NewRequest(&modulev1.GetLabelsRequest{LabelRefs: []*modulev1.LabelRef{labelName("acme", "weather", "v1.0.0")}}))
	if err != nil || gl.Msg.GetLabels()[0].GetArchiveTime() == nil {
		t.Fatalf("archived label = %v, %v", gl, err)
	}
	// An archived label cannot become the default label.
	_, err = mods.UpdateModules(ctx, connect.NewRequest(&modulev1.UpdateModulesRequest{Values: []*modulev1.UpdateModulesRequest_Value{{
		ModuleRef: modName("acme", "weather"), DefaultLabelName: new("v1.0.0"),
	}}}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if _, err := lc.UnarchiveLabels(ctx, connect.NewRequest(&modulev1.UnarchiveLabelsRequest{LabelRefs: []*modulev1.LabelRef{labelName("acme", "weather", "v1.0.0")}})); err != nil {
		t.Fatal(err)
	}
	if got := list(&modulev1.ListLabelsRequest{ResourceRef: moduleResource("acme", "weather")}); len(got) != 3 {
		t.Fatalf("after unarchive = %v", got)
	}
	// Moving an archived label unarchives it.
	if _, err := lc.ArchiveLabels(ctx, connect.NewRequest(&modulev1.ArchiveLabelsRequest{LabelRefs: []*modulev1.LabelRef{labelName("acme", "weather", "v1.0.0")}})); err != nil {
		t.Fatal(err)
	}
	if l, err := setLabel(t, lc, labelName("acme", "weather", "v1.0.0"), c2.ID); err != nil || l.GetArchiveTime() != nil {
		t.Fatalf("set archived label = %v, %v", l, err)
	}
	// ListLabelHistory is not implemented yet.
	_, err = lc.ListLabelHistory(ctx, connect.NewRequest(&modulev1.ListLabelHistoryRequest{LabelRef: labelName("acme", "weather", "main")}))
	wantCode(t, err, connect.CodeUnimplemented)
}
