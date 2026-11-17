// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"connectrpc.com/connect"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/b5"
	"github.com/marijnkoesen/protodex/internal/blob"
	"github.com/marijnkoesen/protodex/internal/observability"
	"github.com/marijnkoesen/protodex/internal/store"
)

// fileType classifies a module file the way the registry API does.
func fileType(p string) modulev1.FileType {
	switch {
	case path.Ext(p) == ".proto":
		return modulev1.FileType_FILE_TYPE_PROTO
	case p == "LICENSE":
		return modulev1.FileType_FILE_TYPE_LICENSE
	default:
		return modulev1.FileType_FILE_TYPE_DOC
	}
}

// contentReader reads the files of stored commits.
type contentReader struct {
	blobs blob.Store
	obs   *observability.Observability
}

// readBlob returns the content of a blob: inline in the store (mirrors may
// keep small files there) or in the blob store.
func (r contentReader) readBlob(ctx context.Context, q store.Querier, digest string) ([]byte, error) {
	b, err := q.GetBlob(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", digest, err)
	}
	if len(b.Data) > 0 || b.Size == 0 {
		return b.Data, nil
	}
	d, err := blob.ParseDigest(digest)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Grow(int(min(b.Size, 64<<20)))
	err = r.obs.Blob(ctx, "Get", func(ctx context.Context) error {
		rc, err := r.blobs.Get(ctx, d)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		_, err = io.Copy(&buf, rc)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", digest, err)
	}
	return buf.Bytes(), nil
}

// files returns the files of a commit, sorted by path, keeping only those
// keep accepts (all when keep is nil).
func (r contentReader) files(ctx context.Context, q store.Querier, commitID string, keep func(string) bool) ([]b5.File, error) {
	entries, err := q.GetManifest(ctx, commitID)
	if err != nil {
		return nil, fmt.Errorf("manifest of commit %s: %w", commitID, err)
	}
	out := make([]b5.File, 0, len(entries))
	for _, e := range entries {
		if keep != nil && !keep(e.Path) {
			continue
		}
		data, err := r.readBlob(ctx, q, e.BlobDigest)
		if err != nil {
			return nil, err
		}
		out = append(out, b5.File{Path: e.Path, Content: data})
	}
	return out, nil
}

// requireCommit fails when a reference resolved to a module without commits.
func requireCommit(r resolvedRef) error {
	if r.commit == nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("module %s/%s has no commits", r.owner.Name, r.mod.Name))
	}
	return nil
}

// downloadService implements buf.registry.module.v1.DownloadService.
type downloadService struct {
	modulev1connect.UnimplementedDownloadServiceHandler
	st      store.Store
	content contentReader
}

// pathFilter returns a filter for the requested file types and paths (a
// path also selects the files below it), and a check that reports the
// requested paths that matched nothing.
func pathFilter(types []modulev1.FileType, paths []string) (keep func(string) bool, unmatched func() []string) {
	matched := make(map[string]bool, len(paths))
	keep = func(p string) bool {
		if len(types) > 0 && !slices.Contains(types, fileType(p)) {
			return false
		}
		if len(paths) == 0 {
			return true
		}
		ok := false
		for _, want := range paths {
			if p == want || strings.HasPrefix(p, want+"/") {
				matched[want] = true
				ok = true
			}
		}
		return ok
	}
	unmatched = func() []string {
		var out []string
		for _, want := range paths {
			if !matched[want] {
				out = append(out, want)
			}
		}
		return out
	}
	return keep, unmatched
}

// Download returns the files of the commit each reference resolves to (a
// module's default label, a label's commit, or a commit), optionally
// narrowed by file type and path.
func (s *downloadService) Download(ctx context.Context, req *connect.Request[modulev1.DownloadRequest]) (*connect.Response[modulev1.DownloadResponse], error) {
	resp := &modulev1.DownloadResponse{}
	for _, v := range req.Msg.GetValues() {
		r, err := resolveRef(ctx, s.st, v.GetResourceRef())
		if err != nil {
			return nil, err
		}
		if err := requireCommit(r); err != nil {
			return nil, err
		}
		keep, unmatched := pathFilter(v.GetFileTypes(), v.GetPaths())
		files, err := s.content.files(ctx, s.st, r.commit.ID, keep)
		if err != nil {
			return nil, internalErr(err)
		}
		if missing := unmatched(); len(missing) > 0 && !v.GetPathsAllowNotExist() {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("paths %q not found in commit %s", missing, r.commit.ID))
		}
		content := &modulev1.DownloadResponse_Content{Commit: commitProto(r.mod, *r.commit)}
		for _, f := range files {
			content.Files = append(content.Files, &modulev1.File{Path: f.Path, Content: f.Content})
		}
		resp.Contents = append(resp.Contents, content)
	}
	return connect.NewResponse(resp), nil
}
