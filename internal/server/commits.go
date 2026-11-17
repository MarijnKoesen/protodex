// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/b5"
	"github.com/marijnkoesen/protodex/internal/store"
)

// commitProto converts a stored commit. Digests are b5; a digest in any
// other form (which the store never holds) is left out.
func commitProto(m store.Module, c store.Commit) *modulev1.Commit {
	p := &modulev1.Commit{
		Id:         c.ID,
		CreateTime: timestamppb.New(c.CreatedAt),
		OwnerId:    m.OwnerID,
		ModuleId:   m.ID,
	}
	if v, err := b5.Bytes(c.Digest); err == nil {
		p.Digest = &modulev1.Digest{Type: modulev1.DigestType_DIGEST_TYPE_B5, Value: v}
	}
	return p
}

func commitNotFound(what string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("commit %s not found", what))
}

func getCommit(ctx context.Context, q store.Querier, m store.Module, id string) (store.Commit, error) {
	c, err := q.GetCommit(ctx, m.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Commit{}, commitNotFound(id)
	}
	if err != nil {
		return store.Commit{}, internalErr(err)
	}
	return c, nil
}

// refKind says what a ResourceRef names.
type refKind int

const (
	refModule refKind = iota
	refLabel
	refCommit
)

// resolvedRef is a ResourceRef resolved to a readable module and, unless it
// names a module with no default label yet, the commit it points at.
type resolvedRef struct {
	kind   refKind
	mod    store.Module
	commit *store.Commit
}

// labelRef resolves a label to the commit it points at.
func labelRef(ctx context.Context, q store.Querier, l labeled) (resolvedRef, error) {
	c, err := getCommit(ctx, q, l.mod, l.label.CommitID)
	return resolvedRef{refLabel, l.mod, &c}, err
}

// moduleRef resolves a module to its default label's commit, if any.
func moduleRef(ctx context.Context, q store.Querier, o store.Owner, m store.Module) (resolvedRef, error) {
	r := resolvedRef{kind: refModule, mod: m}
	l, err := getLabel(ctx, q, o, m, m.DefaultLabel)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	c, err := getCommit(ctx, q, m, l.label.CommitID)
	r.commit = &c
	return r, err
}

// resolveRef resolves a ResourceRef: a commit id, a label, or a module. A
// name's ref is a commit id or, failing that, a label name.
func resolveRef(ctx context.Context, q store.Querier, ref *modulev1.ResourceRef) (resolvedRef, error) {
	if id := ref.GetId(); id != "" {
		c, err := q.GetCommitByID(ctx, id)
		switch {
		case err == nil:
			_, m, err := getModuleByID(ctx, q, c.ModuleID)
			if connect.CodeOf(err) == connect.CodeNotFound {
				return resolvedRef{}, commitNotFound(id)
			}
			return resolvedRef{refCommit, m, &c}, err
		case !errors.Is(err, store.ErrNotFound):
			return resolvedRef{}, internalErr(err)
		}
		if l, err := getLabelByID(ctx, q, id); err == nil {
			return labelRef(ctx, q, l)
		} else if connect.CodeOf(err) != connect.CodeNotFound {
			return resolvedRef{}, err
		}
		o, m, err := getModuleByID(ctx, q, id)
		if err != nil {
			return resolvedRef{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("resource with id %s not found", id))
		}
		return moduleRef(ctx, q, o, m)
	}
	n := ref.GetName()
	if n == nil {
		return resolvedRef{}, invalidArg("resource reference is empty")
	}
	o, m, err := getModuleByName(ctx, q, n.GetOwner(), n.GetModule())
	if err != nil {
		return resolvedRef{}, err
	}
	if name := n.GetLabelName(); name != "" {
		l, err := getLabel(ctx, q, o, m, name)
		if err != nil {
			return resolvedRef{}, err
		}
		return labelRef(ctx, q, l)
	}
	if name := n.GetRef(); name != "" {
		switch c, err := q.GetCommit(ctx, m.ID, name); {
		case err == nil:
			return resolvedRef{refCommit, m, &c}, nil
		case !errors.Is(err, store.ErrNotFound):
			return resolvedRef{}, internalErr(err)
		}
		l, err := getLabel(ctx, q, o, m, name)
		if err != nil {
			return resolvedRef{}, commitNotFound(name)
		}
		return labelRef(ctx, q, l)
	}
	return moduleRef(ctx, q, o, m)
}

// commitService implements buf.registry.module.v1.CommitService.
type commitService struct {
	modulev1connect.UnimplementedCommitServiceHandler
	st store.Store
}

// GetCommits resolves each reference to a commit, in order.
func (s *commitService) GetCommits(ctx context.Context, req *connect.Request[modulev1.GetCommitsRequest]) (*connect.Response[modulev1.GetCommitsResponse], error) {
	resp := &modulev1.GetCommitsResponse{}
	for _, ref := range req.Msg.GetResourceRefs() {
		r, err := resolveRef(ctx, s.st, ref)
		if err != nil {
			return nil, err
		}
		if r.commit == nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("module %s has no commits", r.mod.Name))
		}
		resp.Commits = append(resp.Commits, commitProto(r.mod, *r.commit))
	}
	return connect.NewResponse(resp), nil
}

// ListCommits lists the commits a reference selects: all commits of a
// module, the commits up to and including a label's commit (label history is
// not recorded), or the single commit named. id_query matches id prefixes.
func (s *commitService) ListCommits(ctx context.Context, req *connect.Request[modulev1.ListCommitsRequest]) (*connect.Response[modulev1.ListCommitsResponse], error) {
	msg := req.Msg
	r, err := resolveRef(ctx, s.st, msg.GetResourceRef())
	if err != nil {
		return nil, err
	}
	var cs []store.Commit
	switch r.kind {
	case refCommit:
		cs = []store.Commit{*r.commit}
	default:
		if cs, err = s.st.ListCommits(ctx, r.mod.ID); err != nil {
			return nil, internalErr(err)
		}
		if r.kind == refLabel {
			cs = slices.DeleteFunc(cs, func(x store.Commit) bool { return x.CreatedAt.After(r.commit.CreatedAt) })
		}
	}
	if q := strings.ToLower(msg.GetIdQuery()); q != "" {
		cs = slices.DeleteFunc(cs, func(c store.Commit) bool { return !strings.HasPrefix(c.ID, q) })
	}
	// The store lists newest first.
	if msg.GetOrder() == modulev1.ListCommitsRequest_ORDER_CREATE_TIME_ASC {
		slices.Reverse(cs)
	}
	cs, next, err := page(cs, msg.GetPageSize(), msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	resp := &modulev1.ListCommitsResponse{NextPageToken: next}
	for _, c := range cs {
		resp.Commits = append(resp.Commits, commitProto(r.mod, c))
	}
	return connect.NewResponse(resp), nil
}
