// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"connectrpc.com/connect"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/store"
)

// resourceService implements buf.registry.module.v1.ResourceService.
type resourceService struct {
	modulev1connect.UnimplementedResourceServiceHandler
	st store.Store
}

// GetResources resolves each reference (as CommitService and
// DownloadService do) and returns what it names: a module, a label, or a
// commit. A name with a ref (owner/module:ref) is a commit when ref is a
// commit id of the module, otherwise a label.
func (s *resourceService) GetResources(ctx context.Context, req *connect.Request[modulev1.GetResourcesRequest]) (*connect.Response[modulev1.GetResourcesResponse], error) {
	resp := &modulev1.GetResourcesResponse{}
	for _, ref := range req.Msg.GetResourceRefs() {
		r, err := resolveRef(ctx, s.st, ref)
		if err != nil {
			return nil, err
		}
		res := &modulev1.Resource{}
		switch r.kind {
		case refModule:
			res.Value = &modulev1.Resource_Module{Module: moduleProto(r.mod)}
		case refLabel:
			l, err := labelProto(ctx, s.st, *r.label)
			if err != nil {
				return nil, err
			}
			res.Value = &modulev1.Resource_Label{Label: l}
		case refCommit:
			res.Value = &modulev1.Resource_Commit{Commit: commitProto(r.mod, *r.commit)}
		}
		resp.Resources = append(resp.Resources, res)
	}
	return connect.NewResponse(resp), nil
}
