// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"path"

	"connectrpc.com/connect"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/fdset"
	"github.com/marijnkoesen/protodex/internal/store"
)

// defaultDescriptorCacheBytes bounds the FileDescriptorSet cache.
const defaultDescriptorCacheBytes = 64 << 20

// fileDescriptorSetService implements
// buf.registry.module.v1.FileDescriptorSetService. Sets are compiled on
// demand from the commit's files and those of its dependency commits, and
// cached by commit digest and options.
type fileDescriptorSetService struct {
	modulev1connect.UnimplementedFileDescriptorSetServiceHandler
	st      store.Store
	content contentReader
	cache   *fdset.Cache
}

func isProto(p string) bool { return path.Ext(p) == ".proto" }

// sources loads the .proto files of root and of its transitive
// dependencies. It returns all of them by path and the paths of root's own
// files. A path present in two modules is an error, as it is for buf.
func (s *fileDescriptorSetService) sources(ctx context.Context, root depCommit) (map[string]string, []string, error) {
	srcs := map[string]string{}
	owner := map[string]string{} // path -> module name, for error messages
	var targets []string
	err := walkDeps(ctx, s.st, []depCommit{root}, func(c depCommit) error {
		files, err := s.content.files(ctx, s.st, c.commit.ID, isProto)
		if err != nil {
			return internalErr(err)
		}
		for _, f := range files {
			if prev, dup := owner[f.Path]; dup {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("file %s is in both module %s and module %s", f.Path, prev, c.mod.Name))
			}
			owner[f.Path] = c.mod.Name
			srcs[f.Path] = string(f.Content)
			if c.commit.ID == root.commit.ID {
				targets = append(targets, f.Path)
			}
		}
		return nil
	}, nil)
	return srcs, targets, err
}

// GetFileDescriptorSet compiles the commit the reference resolves to.
// Filtering by type (include_types, exclude_types) is not supported yet.
func (s *fileDescriptorSetService) GetFileDescriptorSet(ctx context.Context, req *connect.Request[modulev1.GetFileDescriptorSetRequest]) (*connect.Response[modulev1.GetFileDescriptorSetResponse], error) {
	msg := req.Msg
	if len(msg.GetIncludeTypes()) > 0 || len(msg.GetExcludeTypes()) > 0 {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("include_types and exclude_types are not supported"))
	}
	r, err := resolveRef(ctx, s.st, msg.GetResourceRef())
	if err != nil {
		return nil, err
	}
	if err := requireCommit(r); err != nil {
		return nil, err
	}
	// The digest pins the files and the dependency digests; fall back to
	// the commit id should a commit ever lack one.
	cacheID := r.commit.Digest
	if cacheID == "" {
		cacheID = "commit:" + r.commit.ID
	}
	key := fdset.Key{Digest: cacheID, Opts: fdset.Options{
		ExcludeImports:         msg.GetExcludeImports(),
		SourceCodeInfo:         msg.GetIncludeSourceCodeInfo(),
		SourceRetentionOptions: msg.GetIncludeSourceRetentionOptions(),
	}}
	set, ok := s.cache.Get(key)
	if !ok {
		srcs, targets, err := s.sources(ctx, depCommit{r.mod, *r.commit})
		if err != nil {
			return nil, err
		}
		if len(targets) == 0 {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("commit %s has no .proto files", r.commit.ID))
		}
		set, err = fdset.Build(ctx, srcs, targets, key.Opts)
		var ce *fdset.CompileError
		switch {
		case errors.As(err, &ce):
			return nil, connect.NewError(connect.CodeFailedPrecondition, ce)
		case err != nil:
			return nil, internalErr(err)
		}
		s.cache.Put(key, set)
	}
	return connect.NewResponse(&modulev1.GetFileDescriptorSetResponse{
		FileDescriptorSet: set,
		Commit:            commitProto(r.mod, *r.commit),
	}), nil
}
