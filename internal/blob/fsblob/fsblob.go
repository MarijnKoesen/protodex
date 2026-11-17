// SPDX-License-Identifier: Apache-2.0

// Package fsblob is a filesystem blob.Store for tests and development.
package fsblob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/marijnkoesen/protodex/internal/blob"
)

// Store keeps blobs under Root as sha256/<ab>/<hex>.
type Store struct {
	root    string
	maxSize int64
}

var (
	_ blob.Store  = (*Store)(nil)
	_ blob.Lister = (*Store)(nil)
)

// New creates a store rooted at dir. maxSize <= 0 selects blob.DefaultMaxSize.
func New(dir string, maxSize int64) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o750); err != nil {
		return nil, err
	}
	return &Store{root: dir, maxSize: maxSize}, nil
}

func (s *Store) path(d blob.Digest) (string, error) {
	if d.IsZero() {
		return "", blob.ErrInvalidDigest
	}
	h := d.Hex()
	return filepath.Join(s.root, "sha256", h[:2], h), nil
}

// Put implements blob.Store.
func (s *Store) Put(ctx context.Context, d blob.Digest, r io.Reader) (err error) {
	dst, err := s.path(d)
	if err != nil {
		return err
	}
	if ok, _ := s.Exists(ctx, d); ok {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "put-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	v := blob.NewVerifier(&ctxReader{ctx, r}, s.maxSize)
	if _, err = io.Copy(tmp, v); err != nil {
		return err
	}
	if err = v.Check(d); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// Get implements blob.Store.
func (s *Store) Get(_ context.Context, d blob.Digest) (io.ReadCloser, error) {
	p, err := s.path(d)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p) //nolint:gosec // path derived from validated digest
	if errors.Is(err, fs.ErrNotExist) {
		return nil, blob.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("fsblob: %w", err)
	}
	return f, nil
}

// Exists implements blob.Store.
func (s *Store) Exists(_ context.Context, d blob.Digest) (bool, error) {
	p, err := s.path(d)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Delete implements blob.Store.
func (s *Store) Delete(_ context.Context, d blob.Digest) error {
	p, err := s.path(d)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// List implements blob.Lister. Files that are not blobs (wrong place or
// name) are skipped, and so are the in-progress uploads under tmp/.
func (s *Store) List(ctx context.Context, fn func(blob.Info) error) error {
	root := filepath.Join(s.root, "sha256")
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed concurrently, or nothing stored yet
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.IsDir() || !e.Type().IsRegular() {
			return nil
		}
		d, perr := blob.ParseDigest("sha256:" + e.Name())
		if perr != nil || filepath.Base(filepath.Dir(p)) != d.Hex()[:2] {
			return nil
		}
		fi, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return fn(blob.Info{Digest: d, Size: fi.Size(), ModTime: fi.ModTime()})
	})
	if err != nil {
		return fmt.Errorf("fsblob: list: %w", err)
	}
	return nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
