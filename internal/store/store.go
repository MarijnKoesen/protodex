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

// Owner types.
const (
	OwnerUser         = "user"
	OwnerOrganization = "organization"
)

// Owner is a user or organization that owns modules. Names are unique across
// all owners, whatever their type. IDs never change; names may be renamed.
type Owner struct {
	ID   string
	Name string
	Type string // OwnerUser or OwnerOrganization
	// Mirror marks an organization that reserves an upstream owner name
	// (e.g. googleapis) for mirrored modules. Mirrors cannot be renamed.
	Mirror    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Module visibilities.
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// Module is a repository of protobuf files.
type Module struct {
	ID           string
	OwnerID      string
	Name         string
	Visibility   string // VisibilityPublic or VisibilityPrivate
	DefaultLabel string
	Description  string
	URL          string
	Deprecated   bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Commit is an immutable snapshot of a module.
type Commit struct {
	ID        string
	ModuleID  string
	Digest    string // e.g. "b5:<hex>"; passed through for mirrors
	CreatedAt time.Time
}

// Label is a movable pointer to a commit. Its ID is assigned on creation and
// kept when the label moves.
type Label struct {
	ID       string
	ModuleID string
	Name     string
	CommitID string
	Archived bool
	// ArchivedAt is set while the label is archived.
	ArchivedAt *time.Time
	// UpdatedBy is the owner ID of the user that last moved the label, or
	// empty when unknown.
	UpdatedBy string
	CreatedAt time.Time
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

// Blob is content-addressed file content. When the bytes live in the blob
// store, Data is empty and Size records their length.
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

// Identity links an external (OIDC) subject to a user owner. Role is the
// access role last derived from the IdP's claims.
type Identity struct {
	Issuer      string
	Subject     string
	OwnerID     string
	Email       string
	Role        string
	CreatedAt   time.Time
	LastLoginAt time.Time
}

// Querier is the set of data operations, available both on a Store and
// inside a transaction. Get/List methods return ErrNotFound for missing
// single rows; creates return ErrConflict on duplicates or dangling
// references. Create methods fill in CreatedAt/UpdatedAt when zero.
type Querier interface {
	CreateOwner(ctx context.Context, o Owner) (Owner, error)
	GetOwner(ctx context.Context, name string) (Owner, error)
	GetOwnerByID(ctx context.Context, id string) (Owner, error)
	ListOwners(ctx context.Context) ([]Owner, error) // sorted by name
	// RenameOwner changes an owner's name, keeping its ID (and so its
	// modules and tokens). It returns ErrNotFound for an unknown id and
	// ErrConflict when the name is taken or the owner is a mirror.
	RenameOwner(ctx context.Context, id, newName string) (Owner, error)

	CreateModule(ctx context.Context, m Module) (Module, error)
	GetModule(ctx context.Context, ownerID, name string) (Module, error)
	GetModuleByID(ctx context.Context, id string) (Module, error)
	ListModules(ctx context.Context, ownerID string) ([]Module, error) // sorted by name
	ListAllModules(ctx context.Context) ([]Module, error)              // sorted by owner ID, name
	// UpdateModule stores m's Visibility, DefaultLabel, Description, URL and
	// Deprecated, setting UpdatedAt to now when zero. It returns the stored
	// module, or ErrNotFound for an unknown ID.
	UpdateModule(ctx context.Context, m Module) (Module, error)
	DeleteModule(ctx context.Context, id string) error

	CreateCommit(ctx context.Context, c Commit) (Commit, error)
	GetCommit(ctx context.Context, moduleID, id string) (Commit, error)
	GetCommitByID(ctx context.Context, id string) (Commit, error)
	ListCommits(ctx context.Context, moduleID string) ([]Commit, error) // newest first

	// SetLabel creates the label or moves it to another commit, setting
	// Archived, UpdatedBy and UpdatedAt. An existing label keeps its ID and
	// CreatedAt. It returns the stored label.
	SetLabel(ctx context.Context, l Label) (Label, error)
	GetLabel(ctx context.Context, moduleID, name string) (Label, error)
	GetLabelByID(ctx context.Context, id string) (Label, error)
	ListLabels(ctx context.Context, moduleID string) ([]Label, error) // sorted by name
	// ArchiveLabel archives or unarchives a label (ErrNotFound if missing).
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

	CreateIdentity(ctx context.Context, i Identity) (Identity, error)
	GetIdentity(ctx context.Context, issuer, subject string) (Identity, error)
	// UpdateIdentity sets Email, Role and LastLoginAt of an existing identity.
	UpdateIdentity(ctx context.Context, i Identity) error
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
