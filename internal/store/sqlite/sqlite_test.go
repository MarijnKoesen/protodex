// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

func hasColumn(t *testing.T, s *Store, table, column string) bool {
	t.Helper()
	var n int
	err := s.writer.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
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
	if !hasColumn(t, s, "owners", "mirror") {
		t.Fatal("owners.mirror missing after up")
	}
	if err := s.Rollback(ctx); err != nil { // 00003: drops the owner columns
		t.Fatal(err)
	}
	if hasColumn(t, s, "owners", "mirror") {
		t.Fatal("00003 not rolled back cleanly")
	}
	for range 2 { // one step per remaining migration (init, identities)
		if err := s.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
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

func TestSymlinkedDirLockRefusedBeforeFileExists(t *testing.T) {
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "dirlink")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	s := open(t, filepath.Join(realDir, "p.db"))
	defer func() { _ = s.Close() }()
	if _, err := Open(filepath.Join(link, "p.db")); !errors.Is(err, ErrLocked) {
		t.Fatalf("open via symlinked dir: err = %v, want ErrLocked", err)
	}
}

// TestHelperHoldsDB runs as a separate process for TestSecondProcessRefused.
func TestHelperHoldsDB(t *testing.T) {
	path := os.Getenv("PROTODEX_TEST_HOLD_DB")
	if path == "" {
		t.Skip("helper process only")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_, _ = os.Stdout.WriteString("ready\n")
	_, _ = os.Stdin.Read(make([]byte, 1)) // hold until the parent closes stdin
}

func TestSecondProcessRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsDB$") //nolint:gosec // re-exec of the test binary
	cmd.Env = append(os.Environ(), "PROTODEX_TEST_HOLD_DB="+path)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = cmd.Wait() }()
	if _, err := out.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("open while another process holds the db: err = %v, want ErrLocked", err)
	}
}

func TestOwnerMigrationBackfillsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "p.db"))
	defer func() { _ = s.Close() }()
	p, err := s.provider()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writer.ExecContext(ctx, `INSERT INTO owners (id, name, type, created_at) VALUES ('o1', 'acme', 'organization', 42)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	o, err := s.GetOwner(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if o.Mirror || !o.UpdatedAt.Equal(o.CreatedAt) || o.CreatedAt.UnixMicro() != 42 {
		t.Fatalf("migrated owner = %+v", o)
	}
}
