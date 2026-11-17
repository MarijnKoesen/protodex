// SPDX-License-Identifier: Apache-2.0

// Package store defines the metadata persistence interface. Backends
// (SQLite today, Postgres later) must pass internal/store/storetest.
package store

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors returned by every backend.
var (
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict means a uniqueness or foreign-key constraint was violated.
	ErrConflict = errors.New("store: conflict")
)

// Owner is a user or organization that owns modules.
type Owner struct {
	ID        string
	Name      string
	Type      string // "user" or "organization"
	CreatedAt time.Time
}

// Module is a repository of protobuf files.
type Module struct {
	ID           string
	OwnerID      string
	Name         string
	Visibility   string // "public" or "private"
	DefaultLabel string
	CreatedAt    time.Time
}

// Commit is an immutable snapshot of a module.
type Commit struct {
	ID        string
	ModuleID  string
	Digest    string // e.g. "b5:<hex>"; passed through for mirrors
	CreatedAt time.Time
}

// Label is a movable pointer to a commit.
type Label struct {
	ModuleID  string
	Name      string
	CommitID  string
	Archived  bool
	UpdatedAt time.Time
}

// CommitDep is a dependency edge from a commit to another (possibly
// foreign-registry) commit.
type CommitDep struct {
	CommitID    string
	DepRegistry string // host; empty means this registry
	DepOwner    string
	DepModule   string
	DepCommitID string
	DepDigest   string
}

// Blob is content-addressed file content.
type Blob struct {
	Digest string
	Size   int64
	Data   []byte
}

// ManifestEntry maps a path in a commit to a blob.
type ManifestEntry struct {
	CommitID   string
	Path       string
	BlobDigest string
}

// Token is an API credential. Only a hash of the secret is stored.
type Token struct {
	ID         string
	OwnerID    string
	Name       string
	Hash       string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
}

// Querier is the set of data operations, available both on a Store and
// inside a transaction. Get/List methods return ErrNotFound for missing
// single rows; creates return ErrConflict on duplicates or dangling
// references. Create methods fill in CreatedAt/UpdatedAt when zero.
type Querier interface {
	CreateOwner(ctx context.Context, o Owner) (Owner, error)
	GetOwner(ctx context.Context, name string) (Owner, error)
	ListOwners(ctx context.Context) ([]Owner, error)

	CreateModule(ctx context.Context, m Module) (Module, error)
	GetModule(ctx context.Context, ownerID, name string) (Module, error)
	ListModules(ctx context.Context, ownerID string) ([]Module, error)
	DeleteModule(ctx context.Context, id string) error

	CreateCommit(ctx context.Context, c Commit) (Commit, error)
	GetCommit(ctx context.Context, moduleID, id string) (Commit, error)
	ListCommits(ctx context.Context, moduleID string) ([]Commit, error) // newest first

	// SetLabel creates the label or moves it to another commit.
	SetLabel(ctx context.Context, l Label) (Label, error)
	GetLabel(ctx context.Context, moduleID, name string) (Label, error)
	ListLabels(ctx context.Context, moduleID string) ([]Label, error)
	ArchiveLabel(ctx context.Context, moduleID, name string, archived bool) error

	AddCommitDeps(ctx context.Context, deps []CommitDep) error
	ListCommitDeps(ctx context.Context, commitID string) ([]CommitDep, error)

	// PutBlob is idempotent: storing an existing digest is a no-op.
	PutBlob(ctx context.Context, b Blob) error
	GetBlob(ctx context.Context, digest string) (Blob, error)
	SetManifest(ctx context.Context, commitID string, entries []ManifestEntry) error
	GetManifest(ctx context.Context, commitID string) ([]ManifestEntry, error) // sorted by path

	CreateToken(ctx context.Context, t Token) (Token, error)
	GetTokenByHash(ctx context.Context, hash string) (Token, error)
	ListTokens(ctx context.Context, ownerID string) ([]Token, error)
	TouchToken(ctx context.Context, id string, at time.Time) error
	DeleteToken(ctx context.Context, id string) error
}

// Store is a metadata backend.
type Store interface {
	Querier
	// WithTx runs fn atomically. fn's error rolls back; nil commits.
	// fn must not use the Store itself, only the Querier it is given.
	WithTx(ctx context.Context, fn func(Querier) error) error
	// Close releases the database and any process lock.
	Close() error
}
