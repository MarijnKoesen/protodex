// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/marijnkoesen/protodex/internal/b5"
	"github.com/marijnkoesen/protodex/internal/blob"
	"github.com/marijnkoesen/protodex/internal/module"
	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/store"
)

// Importer imports configured upstreams into a store.
type Importer struct {
	Store   store.Store
	Blobs   blob.Store
	Fetcher Fetcher
	Log     *slog.Logger
	// Now returns the import time; default time.Now.
	Now func() time.Time
}

// Result is the outcome of importing one upstream.
type Result struct {
	Name     string
	Commit   store.Commit
	Upstream string // resolved upstream commit hash
	// Created is true when a new commit was written; false when the module
	// already had this exact content.
	Created bool
	// LabelsMoved lists labels that were created or moved.
	LabelsMoved []string
}

// Run imports every upstream of cfg in dependency order. It stops at the
// first failure. Re-running with an unchanged config and unchanged upstreams
// changes nothing.
func (im *Importer) Run(ctx context.Context, cfg *Config) ([]Result, error) {
	order, err := cfg.Order()
	if err != nil {
		return nil, err
	}
	imported := map[string]store.Commit{}
	var results []Result
	for _, u := range order {
		deps := make([]depRef, 0, len(u.Deps))
		for _, d := range u.Deps {
			deps = append(deps, depRef{name: d, commit: imported[d]})
		}
		r, err := im.importOne(ctx, u, deps)
		if err != nil {
			return results, fmt.Errorf("mirror %s: %w", u.Name, err)
		}
		imported[u.Name] = r.Commit
		results = append(results, r)
	}
	return results, nil
}

type depRef struct {
	name   string
	commit store.Commit
}

func (im *Importer) log() *slog.Logger {
	if im.Log != nil {
		return im.Log
	}
	return slog.New(slog.DiscardHandler)
}

// collect returns the module files of the snapshot, preserving paths
// relative to each root.
func collect(fsys fs.FS, u Upstream) ([]b5.File, error) {
	seen := map[string]bool{}
	var files []b5.File
	add := func(p string, data []byte) error {
		if seen[p] {
			return fmt.Errorf("file %q found under more than one root", p)
		}
		seen[p] = true
		files = append(files, b5.File{Path: p, Content: data})
		return nil
	}
	for _, root := range u.Roots {
		root = path.Clean(root)
		sub, err := fs.Sub(fsys, root)
		if err != nil {
			return nil, err
		}
		if st, err := fs.Stat(sub, "."); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("root %q is not a directory in the upstream", root)
		}
		err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || path.Ext(p) != ".proto" || !included(p, u.Include) {
				return nil
			}
			data, err := fs.ReadFile(sub, p)
			if err != nil {
				return err
			}
			return add(p, data)
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, errors.New("no .proto files matched the configured roots and includes")
	}
	// The license travels with the module: the first root's LICENSE, else
	// the repository's.
	var lic []byte
	for _, dir := range append(slices.Clone(u.Roots), ".") {
		if data, err := fs.ReadFile(fsys, path.Join(dir, "LICENSE")); err == nil {
			lic = data
			break
		}
	}
	if lic == nil {
		return nil, errors.New("upstream has no LICENSE file to redistribute")
	}
	if err := add("LICENSE", lic); err != nil {
		return nil, err
	}
	return b5.Filter(files), nil
}

func included(p string, include []string) bool {
	if len(include) == 0 {
		return true
	}
	return slices.ContainsFunc(include, func(prefix string) bool {
		prefix = strings.TrimSuffix(path.Clean(prefix), "/")
		return p == prefix || strings.HasPrefix(p, prefix+"/")
	})
}

// sourceURL is the link served as the commit's source_control_url.
func sourceURL(repo, commit string) string {
	u := strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git")
	if strings.HasPrefix(u, "https://github.com/") || strings.HasPrefix(u, "https://gitlab.com/") {
		return u + "/commit/" + commit
	}
	return u
}

func (im *Importer) importOne(ctx context.Context, u Upstream, deps []depRef) (Result, error) {
	snap, err := im.Fetcher.Fetch(ctx, u.URL, u.Ref)
	if err != nil {
		return Result{}, fmt.Errorf("fetch %s at %s: %w", u.URL, u.Ref, err)
	}
	defer snap.Close()
	if !u.PinnedToTag() && snap.Commit != u.Ref {
		return Result{}, fmt.Errorf("ref %s resolved to commit %s", u.Ref, snap.Commit)
	}
	files, err := collect(snap.FS, u)
	if err != nil {
		return Result{}, err
	}
	depDigests := make([]string, len(deps))
	for i, d := range deps {
		depDigests[i] = d.commit.Digest
	}
	digest, err := b5.Digest(files, depDigests)
	if err != nil {
		return Result{}, fmt.Errorf("computing digest: %w", err)
	}

	blobs := make([]store.Blob, 0, len(files))
	for _, f := range files {
		d := blob.DigestOf(f.Content)
		if err := im.Blobs.Put(ctx, d, bytes.NewReader(f.Content)); err != nil {
			return Result{}, fmt.Errorf("store blob: %w", err)
		}
		blobs = append(blobs, store.Blob{Digest: d.String(), Size: int64(len(f.Content))})
	}

	now := time.Now
	if im.Now != nil {
		now = im.Now
	}
	res := Result{Name: u.Name, Upstream: snap.Commit}
	err = im.Store.WithTx(ctx, func(q store.Querier) error {
		res.Created, res.LabelsMoved = false, nil
		o, _, err := owner.Ensure(ctx, q, owner.Mirror, u.Owner())
		if err != nil {
			return err
		}
		m, err := q.GetModule(ctx, o.ID, u.Module())
		if errors.Is(err, store.ErrNotFound) {
			m, err = q.CreateModule(ctx, store.Module{OwnerID: o.ID, Name: u.Module(),
				Visibility: store.VisibilityPublic, DefaultLabel: module.DefaultLabel,
				URL: strings.TrimSuffix(u.URL, ".git"), Description: "Mirror of " + u.URL})
		}
		if err != nil {
			return err
		}
		existing, err := q.ListCommits(ctx, m.ID)
		if err != nil {
			return err
		}
		var c store.Commit
		if len(existing) > 0 && existing[0].Digest == digest {
			c = existing[0]
		} else {
			t := now()
			if len(existing) > 0 && !t.After(existing[0].CreatedAt) {
				t = existing[0].CreatedAt.Add(time.Microsecond)
			}
			c, err = q.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: digest, CreatedAt: t,
				SourceControlURL: sourceURL(u.URL, snap.Commit)})
			if err != nil {
				return err
			}
			manifest := make([]store.ManifestEntry, len(files))
			for i, f := range files {
				if err := q.PutBlob(ctx, blobs[i]); err != nil {
					return err
				}
				manifest[i] = store.ManifestEntry{CommitID: c.ID, Path: f.Path, BlobDigest: blobs[i].Digest}
			}
			if err := q.SetManifest(ctx, c.ID, manifest); err != nil {
				return err
			}
			cds := make([]store.CommitDep, len(deps))
			for i, d := range deps {
				dep, err := q.GetCommitByID(ctx, d.commit.ID)
				if err != nil {
					return err
				}
				do, dm, err := ownerModule(ctx, q, dep.ModuleID)
				if err != nil {
					return err
				}
				cds[i] = store.CommitDep{CommitID: c.ID, DepOwner: do, DepModule: dm, DepCommitID: dep.ID, DepDigest: dep.Digest}
			}
			if err := q.AddCommitDeps(ctx, cds); err != nil {
				return err
			}
			if err := q.PutMirrorCommit(ctx, store.MirrorCommit{CommitID: c.ID, UpstreamURL: u.URL, UpstreamRef: u.Ref,
				UpstreamCommit: snap.Commit, License: u.License, ImportedAt: t}); err != nil {
				return err
			}
			res.Created = true
		}
		res.Commit = c
		for _, name := range u.LabelNames() {
			cur, err := q.GetLabel(ctx, m.ID, name)
			switch {
			case err == nil && cur.CommitID == c.ID && !cur.Archived:
				continue
			case err != nil && !errors.Is(err, store.ErrNotFound):
				return err
			}
			if _, err := q.SetLabel(ctx, store.Label{ID: cur.ID, ModuleID: m.ID, Name: name, CommitID: c.ID}); err != nil {
				return err
			}
			res.LabelsMoved = append(res.LabelsMoved, name)
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	im.log().Info("mirror", "module", u.Name, "ref", u.Ref, "upstream_commit", snap.Commit,
		"commit", res.Commit.ID, "created", res.Created, "labels_moved", res.LabelsMoved)
	return res, nil
}

func ownerModule(ctx context.Context, q store.Querier, moduleID string) (ownerName, moduleName string, err error) {
	m, err := q.GetModuleByID(ctx, moduleID)
	if err != nil {
		return "", "", err
	}
	o, err := q.GetOwnerByID(ctx, m.OwnerID)
	if err != nil {
		return "", "", err
	}
	return o.Name, m.Name, nil
}
