// SPDX-License-Identifier: Apache-2.0

// Package sqlite implements store.Store on SQLite (pure Go, modernc.org/sqlite).
//
// Single-writer discipline: all writes and transactions go through one
// connection (BEGIN IMMEDIATE); reads use a separate pool (WAL allows
// concurrent readers). A lock file next to the database makes a second
// process fail fast instead of fighting over the file.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/marijnkoesen/protodex/internal/store"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrLocked means another process holds the database.
var ErrLocked = errors.New("sqlite: database locked")

// Store is the SQLite backend.
type Store struct {
	queries
	writer, reader *sql.DB
	lock           *os.File
}

var _ store.Store = (*Store)(nil)

func dsn(path string, write bool) string {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	if write {
		q.Set("_txlock", "immediate")
	}
	return "file:" + url.PathEscape(path) + "?" + q.Encode()
}

// Open locks and opens the database at path, creating it if needed. It does
// not migrate; call Migrate.
func Open(path string) (*Store, error) {
	// Lock on the resolved path so symlink aliases share one lock.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	lock, err := acquireLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	s := &Store{lock: lock}
	fail := func(err error) (*Store, error) {
		_ = s.Close()
		return nil, err
	}
	if s.writer, err = sql.Open("sqlite", dsn(path, true)); err != nil {
		return fail(err)
	}
	s.writer.SetMaxOpenConns(1)
	if err := s.writer.Ping(); err != nil {
		return fail(err)
	}
	if s.reader, err = sql.Open("sqlite", dsn(path, false)); err != nil {
		return fail(err)
	}
	s.queries = *newQueries(s.reader, s.writer)
	return s, nil
}

func (s *Store) provider() (*goose.Provider, error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectSQLite3, s.writer, sub)
}

// Migrate applies all pending migrations. It is idempotent.
func (s *Store) Migrate(ctx context.Context) error {
	p, err := s.provider()
	if err != nil {
		return err
	}
	_, err = p.Up(ctx)
	return err
}

// Rollback reverts the most recently applied migration.
func (s *Store) Rollback(ctx context.Context) error {
	p, err := s.provider()
	if err != nil {
		return err
	}
	_, err = p.Down(ctx)
	if errors.Is(err, goose.ErrNoNextVersion) {
		return nil
	}
	return err
}

// WithTx implements store.Store.
func (s *Store) WithTx(ctx context.Context, fn func(store.Querier) error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed { // error or panic: never strand the single writer
			_ = tx.Rollback()
		}
	}()
	if err := fn(newQueries(tx, tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// SetManifest replaces a commit's manifest atomically.
func (s *Store) SetManifest(ctx context.Context, commitID string, entries []store.ManifestEntry) error {
	return s.WithTx(ctx, func(q store.Querier) error { return q.SetManifest(ctx, commitID, entries) })
}

// Close implements store.Store.
func (s *Store) Close() error {
	var errs []error
	if s.reader != nil {
		errs = append(errs, s.reader.Close())
	}
	if s.writer != nil {
		errs = append(errs, s.writer.Close())
	}
	if s.lock != nil {
		errs = append(errs, s.lock.Close()) // closing releases the flock
	}
	return errors.Join(errs...)
}
