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
	Mirror bool
	// Role is the instance role of a user (RoleViewer, RoleMember or
	// RoleAdmin); empty for organizations. Creating a user with an empty
	// role makes it a member.
	Role      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Instance roles of users, in ascending order of privilege.
const (
	// RoleViewer may read but never write, unless granted a module role.
	RoleViewer = "viewer"
	// RoleMember may write its own namespace and organization namespaces.
	RoleMember = "member"
	// RoleAdmin may read and write every module (except mirrors).
	RoleAdmin = "admin"
)

// Module contributor roles, in ascending order of privilege.
const (
	// ContributorRead lets a user read a (private) module.
	ContributorRead = "read"
	// ContributorWrite also lets the user push and manage labels.
	ContributorWrite = "write"
	// ContributorAdmin also lets the user change settings and delete the module.
	ContributorAdmin = "admin"
)

// ModuleContributor grants a user a role on one module.
type ModuleContributor struct {
	ModuleID  string
	UserID    string
	Role      string // ContributorRead, ContributorWrite or ContributorAdmin
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
	// SourceControlURL links the commit to its source (mirrors: the upstream
	// commit). Empty when unknown.
	SourceControlURL string
}

// MirrorCommit records where a mirrored commit came from.
type MirrorCommit struct {
	CommitID       string
	UpstreamURL    string // git URL of the upstream repository
	UpstreamRef    string // the pinned tag or commit from the mirror config
	UpstreamCommit string // resolved upstream commit hash
	License        string // SPDX identifier recorded at import
	ImportedAt     time.Time
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
	// SetOwnerRole sets the instance role of a user. It returns ErrNotFound
	// when id is not a user.
	SetOwnerRole(ctx context.Context, id, role string) error

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

	// SetModuleContributor grants or changes a user's role on a module,
	// returning the stored grant (CreatedAt is kept on change). Unknown
	// modules or users return ErrConflict.
	SetModuleContributor(ctx context.Context, c ModuleContributor) (ModuleContributor, error)
	GetModuleContributor(ctx context.Context, moduleID, userID string) (ModuleContributor, error)
	ListModuleContributors(ctx context.Context, moduleID string) ([]ModuleContributor, error) // sorted by user ID
	ListUserContributions(ctx context.Context, userID string) ([]ModuleContributor, error)    // sorted by module ID
	DeleteModuleContributor(ctx context.Context, moduleID, userID string) error               // ErrNotFound if missing

	CreateCommit(ctx context.Context, c Commit) (Commit, error)
	GetCommit(ctx context.Context, moduleID, id string) (Commit, error)
	GetCommitByID(ctx context.Context, id string) (Commit, error)
	ListCommits(ctx context.Context, moduleID string) ([]Commit, error) // newest first

	// PutMirrorCommit records the provenance of a mirrored commit.
	// GetMirrorCommit returns ErrNotFound for commits that were not
	// mirrored; ListMirrorCommits is newest commit first.
	PutMirrorCommit(ctx context.Context, m MirrorCommit) error
	GetMirrorCommit(ctx context.Context, commitID string) (MirrorCommit, error)
	ListMirrorCommits(ctx context.Context, moduleID string) ([]MirrorCommit, error)

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

	// Blob garbage collection (see package gc). ListBlobDigests returns
	// every blob row; ListReferencedBlobDigests the distinct digests some
	// manifest references (both sorted). DeleteBlob removes a blob row only
	// while no manifest references it, returning ErrConflict otherwise;
	// deleting a missing row is not an error.
	ListBlobDigests(ctx context.Context) ([]string, error)
	ListReferencedBlobDigests(ctx context.Context) ([]string, error)
	BlobReferenced(ctx context.Context, digest string) (bool, error)
	DeleteBlob(ctx context.Context, digest string) error

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
