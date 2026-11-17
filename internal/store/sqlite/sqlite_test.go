// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/storetest"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s := open(t, filepath.Join(t.TempDir(), "p.db"))
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func tableCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	err := s.writer.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'owners'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrateUpDownIdempotent(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "p.db"))
	defer func() { _ = s.Close() }()
	for range 2 { // idempotent
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if tableCount(t, s) != 1 {
		t.Fatal("owners table missing after up")
	}
	if err := s.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if tableCount(t, s) != 0 {
		t.Fatal("owners table present after down")
	}
	if err := s.Rollback(ctx); err != nil { // nothing left to roll back
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil { // up again after down
		t.Fatal(err)
	}
	if tableCount(t, s) != 1 {
		t.Fatal("owners table missing after re-up")
	}
}

func TestSecondOpenRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	s := open(t, path)
	if _, err := Open(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open error = %v, want ErrLocked", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_ = open(t, path).Close() // lock released by Close
}

func TestSymlinkAliasLockRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "p.db")
	s := open(t, target)
	defer func() { _ = s.Close() }()
	link := filepath.Join(dir, "alias.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := Open(link); !errors.Is(err, ErrLocked) {
		t.Fatalf("Open via symlink error = %v, want ErrLocked", err)
	}
}
