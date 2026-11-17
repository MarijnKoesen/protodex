// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/b5"
	"github.com/marijnkoesen/protodex/internal/blob"
	"github.com/marijnkoesen/protodex/internal/gc"
	"github.com/marijnkoesen/protodex/internal/module"
	"github.com/marijnkoesen/protodex/internal/observability"
	"github.com/marijnkoesen/protodex/internal/store"
)

// UploadLimits bound a single push. Zero fields select the defaults.
type UploadLimits struct {
	// MaxFiles is the maximum number of files per module (default 5000).
	MaxFiles int
	// MaxFileBytes is the maximum size of one file (default 10 MiB).
	MaxFileBytes int64
	// MaxTotalBytes is the maximum total file bytes of one Upload request
	// across all its modules (default 64 MiB).
	MaxTotalBytes int64
}

const (
	defaultMaxFiles     = 5000
	defaultMaxFileBytes = 10 << 20
	defaultMaxPushBytes = 64 << 20
)

func (l UploadLimits) withDefaults() UploadLimits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = defaultMaxFiles
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = defaultMaxFileBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = defaultMaxPushBytes
	}
	return l
}

// uploadService implements buf.registry.module.v1.UploadService.
//
// A push is verified and stored in three steps: the request is validated and
// every module and dependency is resolved read-only (so unauthorised or
// invalid pushes store nothing), the file contents go to the blob store
// (content-addressed, so identical files are stored once), and finally one
// metadata transaction writes commits, manifests, dependency edges and label
// moves. Blobs written by a push that then fails are unreferenced but
// harmless until garbage collection (package gc) removes them. To keep the
// collector from deleting a blob between the blob write and the commit that
// references it (identical content may already be stored, unreferenced and
// old), the push pins its blob digests for that whole window. The commit digest is computed by the server (b5, see package b5)
// from the files and the digests of the dependency commits.
type uploadService struct {
	modulev1connect.UnimplementedUploadServiceHandler
	st     store.Store
	blobs  blob.Store
	obs    *observability.Observability
	limits UploadLimits
	leases gc.Leases
}

// pushItem is one validated module of a push.
type pushItem struct {
	owner  store.Owner
	mod    store.Module
	files  []b5.File
	labels []*modulev1.ScopedLabelRef
	deps   []store.CommitDep
	digest string
}

func invalidArg(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// validateFiles rejects duplicate paths and enforces the size limits, then
// returns the files that belong to a module.
func (s *uploadService) validateFiles(files []*modulev1.File) ([]b5.File, error) {
	if len(files) > s.limits.MaxFiles {
		return nil, invalidArg("too many files: %d (limit %d)", len(files), s.limits.MaxFiles)
	}
	seen := make(map[string]bool, len(files))
	all := make([]b5.File, 0, len(files))
	for _, f := range files {
		if seen[f.GetPath()] {
			return nil, invalidArg("duplicate file path %q", f.GetPath())
		}
		seen[f.GetPath()] = true
		if int64(len(f.GetContent())) > s.limits.MaxFileBytes {
			return nil, invalidArg("file %q is %d bytes (limit %d)", f.GetPath(), len(f.GetContent()), s.limits.MaxFileBytes)
		}
		all = append(all, b5.File{Path: f.GetPath(), Content: f.GetContent()})
	}
	kept := b5.Filter(all)
	if len(kept) == 0 {
		return nil, invalidArg("no module files: expected .proto files, LICENSE or a documentation file")
	}
	return kept, nil
}

// resolvePush resolves modules and dependencies and computes digests. It
// only reads, so it can run before and again inside the write transaction.
func (s *uploadService) resolvePush(ctx context.Context, q store.Querier, req *modulev1.UploadRequest, files [][]b5.File) ([]*pushItem, error) {
	items := make([]*pushItem, 0, len(req.GetContents()))
	seenModule := map[string]bool{}
	for i, c := range req.GetContents() {
		o, m, err := resolveModule(ctx, q, c.GetModuleRef())
		if err != nil {
			return nil, err
		}
		if err := requireModule(ctx, q, o, m, permWrite); err != nil {
			return nil, err
		}
		if seenModule[m.ID] {
			return nil, invalidArg("module %s/%s appears more than once in the push", o.Name, m.Name)
		}
		seenModule[m.ID] = true
		items = append(items, &pushItem{owner: o, mod: m, files: files[i], labels: c.GetScopedLabelRefs()})
	}

	// Dependencies must be commits on this server that the caller can read.
	var deps []store.CommitDep
	depModules := map[string]string{} // module id -> commit id
	for _, id := range req.GetDepCommitIds() {
		c, err := q.GetCommitByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("dependency commit %s not found on this registry", id))
		}
		if err != nil {
			return nil, internalErr(err)
		}
		do, dm, err := getModuleByID(ctx, q, c.ModuleID)
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("dependency commit %s not found on this registry", id))
		}
		if err != nil {
			return nil, err
		}
		if prev, dup := depModules[dm.ID]; dup {
			if prev == c.ID {
				continue
			}
			return nil, invalidArg("dependencies %s and %s are both commits of module %s/%s", prev, c.ID, do.Name, dm.Name)
		}
		depModules[dm.ID] = c.ID
		deps = append(deps, store.CommitDep{DepOwner: do.Name, DepModule: dm.Name, DepCommitID: c.ID, DepDigest: c.Digest})
	}
	for _, it := range items {
		if _, self := depModules[it.mod.ID]; self {
			return nil, invalidArg("module %s/%s cannot depend on itself", it.owner.Name, it.mod.Name)
		}
		it.deps = deps
		digests := make([]string, len(deps))
		for i, d := range deps {
			digests[i] = d.DepDigest
		}
		digest, err := b5.Digest(it.files, digests)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("computing digest: %w", err))
		}
		it.digest = digest
	}
	return items, nil
}

// blobDigests returns the distinct blob digests of the items' files.
func blobDigests(items []*pushItem) []string {
	var ds []string
	for _, it := range items {
		for _, f := range it.files {
			ds = append(ds, blob.DigestOf(f.Content).String())
		}
	}
	slices.Sort(ds)
	return slices.Compact(ds)
}

// storeBlobs writes the files to the blob store and returns, per path, the
// blob digest.
func (s *uploadService) storeBlobs(ctx context.Context, items []*pushItem) (map[string]store.Blob, error) {
	blobs := map[string]store.Blob{} // by blob digest
	for _, it := range items {
		for _, f := range it.files {
			d := blob.DigestOf(f.Content)
			if _, done := blobs[d.String()]; done {
				continue
			}
			err := s.obs.Blob(ctx, "Put", func(ctx context.Context) error {
				return s.blobs.Put(ctx, d, bytes.NewReader(f.Content))
			})
			if err != nil {
				return nil, fmt.Errorf("store blob: %w", err)
			}
			blobs[d.String()] = store.Blob{Digest: d.String(), Size: int64(len(f.Content))}
		}
	}
	return blobs, nil
}

// Upload stores one commit per content and moves the requested labels (the
// module's default label when none is given). Pushing content identical to
// the module's latest commit returns that commit instead of a new one.
func (s *uploadService) Upload(ctx context.Context, req *connect.Request[modulev1.UploadRequest]) (*connect.Response[modulev1.UploadResponse], error) {
	me, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	msg := req.Msg
	files := make([][]b5.File, len(msg.GetContents()))
	var total int64
	for i, c := range msg.GetContents() {
		if files[i], err = s.validateFiles(c.GetFiles()); err != nil {
			return nil, err
		}
		for _, f := range files[i] {
			total += int64(len(f.Content))
		}
		if total > s.limits.MaxTotalBytes {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("push is larger than %d bytes", s.limits.MaxTotalBytes))
		}
	}
	// Reject bad pushes before storing anything.
	items, err := s.resolvePush(ctx, s.st, msg, files)
	if err != nil {
		return nil, err
	}
	// Pin before writing: from here until the transaction ends, garbage
	// collection must not delete these blobs.
	release, err := s.leases.Pin(ctx, blobDigests(items))
	if err != nil {
		code := connect.CodeCanceled
		if errors.Is(err, context.DeadlineExceeded) {
			code = connect.CodeDeadlineExceeded
		}
		return nil, connect.NewError(code, err)
	}
	defer release()
	blobs, err := s.storeBlobs(ctx, items)
	if err != nil {
		return nil, internalErr(err)
	}

	resp := &modulev1.UploadResponse{}
	err = s.st.WithTx(ctx, func(q store.Querier) error {
		resp.Commits = resp.Commits[:0]
		// Resolve again: modules and dependencies may have changed since.
		items, err := s.resolvePush(ctx, q, msg, files)
		if err != nil {
			return err
		}
		for _, it := range items {
			c, err := s.commit(ctx, q, it, blobs, me.Owner.ID)
			if err != nil {
				return err
			}
			resp.Commits = append(resp.Commits, commitProto(it.mod, c))
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(resp), nil
}

// commit writes (or finds) the commit for it and moves its labels.
func (s *uploadService) commit(ctx context.Context, q store.Querier, it *pushItem, blobs map[string]store.Blob, userID string) (store.Commit, error) {
	existing, err := q.ListCommits(ctx, it.mod.ID)
	if err != nil {
		return store.Commit{}, internalErr(err)
	}
	var c store.Commit
	if len(existing) > 0 && existing[0].Digest == it.digest {
		c = existing[0] // identical re-push
	} else {
		// Strictly after the latest commit, so "latest" is unambiguous and
		// labels only ever move forward.
		now := time.Now()
		if len(existing) > 0 && !now.After(existing[0].CreatedAt) {
			now = existing[0].CreatedAt.Add(time.Microsecond)
		}
		c, err = q.CreateCommit(ctx, store.Commit{ModuleID: it.mod.ID, Digest: it.digest, CreatedAt: now})
		if err != nil {
			return store.Commit{}, internalErr(err)
		}
		manifest := make([]store.ManifestEntry, 0, len(it.files))
		for _, f := range it.files {
			b := blobs[blob.DigestOf(f.Content).String()]
			if err := q.PutBlob(ctx, b); err != nil {
				return store.Commit{}, internalErr(err)
			}
			manifest = append(manifest, store.ManifestEntry{CommitID: c.ID, Path: f.Path, BlobDigest: b.Digest})
		}
		if err := q.SetManifest(ctx, c.ID, manifest); err != nil {
			return store.Commit{}, internalErr(err)
		}
		deps := slices.Clone(it.deps)
		for i := range deps {
			deps[i].CommitID = c.ID
		}
		if err := q.AddCommitDeps(ctx, deps); err != nil {
			return store.Commit{}, internalErr(err)
		}
	}
	return c, s.moveLabels(ctx, q, it, c, userID)
}

// moveLabels points the requested labels (default: the module's default
// label) at c. Archived labels are unarchived.
func (s *uploadService) moveLabels(ctx context.Context, q store.Querier, it *pushItem, c store.Commit, userID string) error {
	names := make([]string, 0, max(1, len(it.labels)))
	for _, ref := range it.labels {
		name := ref.GetName()
		if id := ref.GetId(); id != "" {
			l, err := q.GetLabelByID(ctx, id)
			if err != nil || l.ModuleID != it.mod.ID {
				return connect.NewError(connect.CodeNotFound, fmt.Errorf("label with id %s not found in module %s/%s", id, it.owner.Name, it.mod.Name))
			}
			name = l.Name
		}
		if err := module.ValidateLabelName(name); err != nil {
			return invalidName(err)
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		names = append(names, it.mod.DefaultLabel)
	}
	for _, name := range names {
		cur, err := q.GetLabel(ctx, it.mod.ID, name)
		switch {
		case err == nil && cur.CommitID == c.ID && !cur.Archived:
			continue
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return internalErr(err)
		}
		if _, err := q.SetLabel(ctx, store.Label{ID: cur.ID, ModuleID: it.mod.ID, Name: name, CommitID: c.ID, UpdatedBy: userID}); err != nil {
			return internalErr(err)
		}
	}
	return nil
}
