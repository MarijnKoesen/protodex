// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/store"
)

// graphService implements buf.registry.module.v1.GraphService from the
// dependency edges recorded at push time. `buf build` and `buf export` of a
// remote module call it even when the module has no dependencies.
type graphService struct {
	modulev1connect.UnimplementedGraphServiceHandler
	st store.Store
}

// depCommit is a commit reached through the dependency graph, with the
// module it belongs to.
type depCommit struct {
	mod    store.Module
	commit store.Commit
}

// walkDeps visits the commits of roots and, transitively, their recorded
// dependencies, each once. visit is called for every commit (roots first,
// then breadth-first) and edge is called for every dependency edge. A
// dependency the caller cannot read is reported as NotFound, so a
// readable module cannot be used to reveal a private one.
func walkDeps(ctx context.Context, q store.Querier, roots []depCommit, visit func(depCommit) error, edge func(from, to string)) error {
	seen := map[string]bool{}
	queue := make([]depCommit, 0, len(roots))
	for _, r := range roots {
		if !seen[r.commit.ID] {
			seen[r.commit.ID] = true
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if err := visit(cur); err != nil {
			return err
		}
		deps, err := q.ListCommitDeps(ctx, cur.commit.ID)
		if err != nil {
			return internalErr(err)
		}
		for _, d := range deps {
			if d.DepRegistry != "" {
				return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("commit %s depends on %s on registry %s: cross-registry dependencies are not supported", cur.commit.ID, d.DepCommitID, d.DepRegistry))
			}
			if edge != nil {
				edge(cur.commit.ID, d.DepCommitID)
			}
			if seen[d.DepCommitID] {
				continue
			}
			seen[d.DepCommitID] = true
			r, err := resolveRef(ctx, q, &modulev1.ResourceRef{Value: &modulev1.ResourceRef_Id{Id: d.DepCommitID}})
			if err != nil {
				return err
			}
			if r.kind != refCommit {
				return commitNotFound(d.DepCommitID)
			}
			queue = append(queue, depCommit{r.mod, *r.commit})
		}
	}
	return nil
}

// GetGraph returns the commits the references resolve to, all their
// transitive dependency commits, and the dependency edges between them.
func (s *graphService) GetGraph(ctx context.Context, req *connect.Request[modulev1.GetGraphRequest]) (*connect.Response[modulev1.GetGraphResponse], error) {
	var roots []depCommit
	for _, ref := range req.Msg.GetResourceRefs() {
		r, err := resolveRef(ctx, s.st, ref)
		if err != nil {
			return nil, err
		}
		if err := requireCommit(r); err != nil {
			return nil, err
		}
		roots = append(roots, depCommit{r.mod, *r.commit})
	}
	g := &modulev1.Graph{}
	err := walkDeps(ctx, s.st, roots,
		func(c depCommit) error {
			g.Commits = append(g.Commits, commitProto(c.mod, c.commit))
			return nil
		},
		func(from, to string) {
			g.Edges = append(g.Edges, &modulev1.Graph_Edge{
				FromNode: &modulev1.Graph_Node{CommitId: from},
				ToNode:   &modulev1.Graph_Node{CommitId: to},
			})
		})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&modulev1.GetGraphResponse{Graph: g}), nil
}
