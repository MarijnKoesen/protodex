// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"

	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite/db"
)

// queries adapts the sqlc-generated db.Queries to store.Querier. r serves
// reads and w serves writes (the same tx in a transaction).
type queries struct{ r, w *db.Queries }

var _ store.Querier = (*queries)(nil)

func newQueries(r, w db.DBTX) *queries { return &queries{r: db.New(r), w: db.New(w)} }

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ts returns t (or now when zero) truncated to the stored microsecond precision.
func ts(t time.Time) time.Time {
	if t.IsZero() {
		t = time.Now()
	}
	return time.UnixMicro(t.UTC().UnixMicro()).UTC()
}

func fromTS(v int64) time.Time { return time.UnixMicro(v).UTC() }

func nullTS(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UTC().UnixMicro(), Valid: true}
}

func ptrTS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromTS(v.Int64)
	return &t
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

const sqliteConstraint = 19 // SQLITE_CONSTRAINT primary result code

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code()&0xff == sqliteConstraint {
		return errors.Join(store.ErrConflict, err)
	}
	return err
}

// existing maps "no rows affected" to ErrNotFound.
func existing(n int64, err error) error {
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func mapAll[A, B any](in []A, err error, f func(A) B) ([]B, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]B, 0, len(in))
	for _, v := range in {
		out = append(out, f(v))
	}
	return out, nil
}

// --- owners ---

func ownerFrom(o db.Owner) store.Owner {
	return store.Owner{ID: o.ID, Name: o.Name, Type: o.Type, Mirror: o.Mirror != 0, Role: o.Role,
		CreatedAt: fromTS(o.CreatedAt), UpdatedAt: fromTS(o.UpdatedAt)}
}

func (q *queries) CreateOwner(ctx context.Context, o store.Owner) (store.Owner, error) {
	if o.ID == "" {
		o.ID = newID()
	}
	o.CreatedAt = ts(o.CreatedAt)
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = o.CreatedAt
	}
	o.UpdatedAt = ts(o.UpdatedAt)
	if o.Type == store.OwnerUser && o.Role == "" {
		o.Role = store.RoleMember
	}
	err := q.w.CreateOwner(ctx, db.CreateOwnerParams{ID: o.ID, Name: o.Name, Type: o.Type, Mirror: b2i(o.Mirror), Role: o.Role,
		CreatedAt: o.CreatedAt.UnixMicro(), UpdatedAt: o.UpdatedAt.UnixMicro()})
	return o, mapErr(err)
}

func (q *queries) GetOwner(ctx context.Context, name string) (store.Owner, error) {
	o, err := q.r.GetOwner(ctx, name)
	return ownerFrom(o), mapErr(err)
}

func (q *queries) GetOwnerByID(ctx context.Context, id string) (store.Owner, error) {
	o, err := q.r.GetOwnerByID(ctx, id)
	return ownerFrom(o), mapErr(err)
}

func (q *queries) ListOwners(ctx context.Context) ([]store.Owner, error) {
	rows, err := q.r.ListOwners(ctx)
	return mapAll(rows, err, ownerFrom)
}

func (q *queries) RenameOwner(ctx context.Context, id, newName string) (store.Owner, error) {
	n, err := q.w.RenameOwner(ctx, db.RenameOwnerParams{Name: newName, UpdatedAt: ts(time.Time{}).UnixMicro(), ID: id})
	if err != nil {
		return store.Owner{}, mapErr(err)
	}
	// Read back through the writer so a transaction sees its own update.
	o, err := q.w.GetOwnerByID(ctx, id)
	if err != nil {
		return store.Owner{}, mapErr(err)
	}
	if n == 0 { // the row exists, so the mirror guard refused it
		return store.Owner{}, fmt.Errorf("%w: mirror owner %q cannot be renamed", store.ErrConflict, o.Name)
	}
	return ownerFrom(o), nil
}

func (q *queries) SetOwnerRole(ctx context.Context, id, role string) error {
	return existing(q.w.SetOwnerRole(ctx, db.SetOwnerRoleParams{Role: role, UpdatedAt: ts(time.Time{}).UnixMicro(), ID: id}))
}

// --- module contributors ---

func contributorFrom(c db.ModuleContributor) store.ModuleContributor {
	return store.ModuleContributor{ModuleID: c.ModuleID, UserID: c.UserID, Role: c.Role,
		CreatedAt: fromTS(c.CreatedAt), UpdatedAt: fromTS(c.UpdatedAt)}
}

func (q *queries) SetModuleContributor(ctx context.Context, c store.ModuleContributor) (store.ModuleContributor, error) {
	c.UpdatedAt = ts(c.UpdatedAt)
	if c.CreatedAt.IsZero() {
		c.CreatedAt = c.UpdatedAt
	}
	err := q.w.SetModuleContributor(ctx, db.SetModuleContributorParams{ModuleID: c.ModuleID, UserID: c.UserID,
		Role: c.Role, CreatedAt: ts(c.CreatedAt).UnixMicro(), UpdatedAt: c.UpdatedAt.UnixMicro()})
	if err != nil {
		return store.ModuleContributor{}, mapErr(err)
	}
	row, err := q.w.GetModuleContributor(ctx, db.GetModuleContributorParams{ModuleID: c.ModuleID, UserID: c.UserID})
	return contributorFrom(row), mapErr(err)
}

func (q *queries) GetModuleContributor(ctx context.Context, moduleID, userID string) (store.ModuleContributor, error) {
	c, err := q.r.GetModuleContributor(ctx, db.GetModuleContributorParams{ModuleID: moduleID, UserID: userID})
	return contributorFrom(c), mapErr(err)
}

func (q *queries) ListModuleContributors(ctx context.Context, moduleID string) ([]store.ModuleContributor, error) {
	rows, err := q.r.ListModuleContributors(ctx, moduleID)
	return mapAll(rows, err, contributorFrom)
}

func (q *queries) ListUserContributions(ctx context.Context, userID string) ([]store.ModuleContributor, error) {
	rows, err := q.r.ListUserContributions(ctx, userID)
	return mapAll(rows, err, contributorFrom)
}

func (q *queries) DeleteModuleContributor(ctx context.Context, moduleID, userID string) error {
	return existing(q.w.DeleteModuleContributor(ctx, db.DeleteModuleContributorParams{ModuleID: moduleID, UserID: userID}))
}

// --- modules ---

func moduleFrom(m db.Module) store.Module {
	return store.Module{ID: m.ID, OwnerID: m.OwnerID, Name: m.Name, Visibility: m.Visibility,
		DefaultLabel: m.DefaultLabel, Description: m.Description, URL: m.Url, Deprecated: m.Deprecated != 0,
		CreatedAt: fromTS(m.CreatedAt), UpdatedAt: fromTS(m.UpdatedAt)}
}

func (q *queries) CreateModule(ctx context.Context, m store.Module) (store.Module, error) {
	if m.ID == "" {
		m.ID = newID()
	}
	m.CreatedAt = ts(m.CreatedAt)
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.CreatedAt
	}
	m.UpdatedAt = ts(m.UpdatedAt)
	err := q.w.CreateModule(ctx, db.CreateModuleParams{ID: m.ID, OwnerID: m.OwnerID, Name: m.Name,
		Visibility: m.Visibility, DefaultLabel: m.DefaultLabel, Description: m.Description, Url: m.URL,
		Deprecated: b2i(m.Deprecated), CreatedAt: m.CreatedAt.UnixMicro(), UpdatedAt: m.UpdatedAt.UnixMicro()})
	return m, mapErr(err)
}

func (q *queries) GetModule(ctx context.Context, ownerID, name string) (store.Module, error) {
	m, err := q.r.GetModule(ctx, db.GetModuleParams{OwnerID: ownerID, Name: name})
	return moduleFrom(m), mapErr(err)
}

func (q *queries) GetModuleByID(ctx context.Context, id string) (store.Module, error) {
	m, err := q.r.GetModuleByID(ctx, id)
	return moduleFrom(m), mapErr(err)
}

func (q *queries) ListAllModules(ctx context.Context) ([]store.Module, error) {
	rows, err := q.r.ListAllModules(ctx)
	return mapAll(rows, err, moduleFrom)
}

func (q *queries) UpdateModule(ctx context.Context, m store.Module) (store.Module, error) {
	err := existing(q.w.UpdateModule(ctx, db.UpdateModuleParams{Visibility: m.Visibility,
		DefaultLabel: m.DefaultLabel, Description: m.Description, Url: m.URL, Deprecated: b2i(m.Deprecated),
		UpdatedAt: ts(m.UpdatedAt).UnixMicro(), ID: m.ID}))
	if err != nil {
		return store.Module{}, err
	}
	// Read back through the writer so a transaction sees its own update.
	row, err := q.w.GetModuleByID(ctx, m.ID)
	return moduleFrom(row), mapErr(err)
}

func (q *queries) ListModules(ctx context.Context, ownerID string) ([]store.Module, error) {
	rows, err := q.r.ListModules(ctx, ownerID)
	return mapAll(rows, err, moduleFrom)
}

// DeleteModule removes the module with its commits, labels, deps and
// manifests. Labels reference commits, so they go first.
func (q *queries) DeleteModule(ctx context.Context, id string) error {
	if err := q.w.DeleteModuleLabels(ctx, id); err != nil {
		return mapErr(err)
	}
	return existing(q.w.DeleteModule(ctx, id))
}

// --- commits ---

func commitFrom(c db.Commit) store.Commit {
	return store.Commit{ID: c.ID, ModuleID: c.ModuleID, Digest: c.Digest, CreatedAt: fromTS(c.CreatedAt)}
}

func (q *queries) CreateCommit(ctx context.Context, c store.Commit) (store.Commit, error) {
	if c.ID == "" {
		c.ID = newID()
	}
	c.CreatedAt = ts(c.CreatedAt)
	err := q.w.CreateCommit(ctx, db.CreateCommitParams{ID: c.ID, ModuleID: c.ModuleID, Digest: c.Digest,
		CreatedAt: c.CreatedAt.UnixMicro()})
	return c, mapErr(err)
}

func (q *queries) GetCommit(ctx context.Context, moduleID, id string) (store.Commit, error) {
	c, err := q.r.GetCommit(ctx, db.GetCommitParams{ModuleID: moduleID, ID: id})
	return commitFrom(c), mapErr(err)
}

func (q *queries) GetCommitByID(ctx context.Context, id string) (store.Commit, error) {
	c, err := q.r.GetCommitByID(ctx, id)
	return commitFrom(c), mapErr(err)
}

func (q *queries) ListCommits(ctx context.Context, moduleID string) ([]store.Commit, error) {
	rows, err := q.r.ListCommits(ctx, moduleID)
	return mapAll(rows, err, commitFrom)
}

// --- labels ---

func labelFrom(l db.Label) store.Label {
	return store.Label{ID: l.ID, ModuleID: l.ModuleID, Name: l.Name, CommitID: l.CommitID,
		Archived: l.Archived != 0, ArchivedAt: ptrTS(l.ArchivedAt), UpdatedBy: l.UpdatedBy,
		CreatedAt: fromTS(l.CreatedAt), UpdatedAt: fromTS(l.UpdatedAt)}
}

func (q *queries) SetLabel(ctx context.Context, l store.Label) (store.Label, error) {
	if l.ID == "" {
		l.ID = newID()
	}
	l.UpdatedAt = ts(l.UpdatedAt)
	if l.CreatedAt.IsZero() {
		l.CreatedAt = l.UpdatedAt
	}
	var archivedAt sql.NullInt64
	if l.Archived {
		archivedAt = nullTS(&l.UpdatedAt)
	}
	err := q.w.SetLabel(ctx, db.SetLabelParams{ID: l.ID, ModuleID: l.ModuleID, Name: l.Name, CommitID: l.CommitID,
		Archived: b2i(l.Archived), ArchivedAt: archivedAt, UpdatedBy: l.UpdatedBy,
		CreatedAt: ts(l.CreatedAt).UnixMicro(), UpdatedAt: l.UpdatedAt.UnixMicro()})
	if err != nil {
		return store.Label{}, mapErr(err)
	}
	// An existing label keeps its id and creation time: read them back.
	row, err := q.w.GetLabel(ctx, db.GetLabelParams{ModuleID: l.ModuleID, Name: l.Name})
	return labelFrom(row), mapErr(err)
}

func (q *queries) GetLabel(ctx context.Context, moduleID, name string) (store.Label, error) {
	l, err := q.r.GetLabel(ctx, db.GetLabelParams{ModuleID: moduleID, Name: name})
	return labelFrom(l), mapErr(err)
}

func (q *queries) GetLabelByID(ctx context.Context, id string) (store.Label, error) {
	l, err := q.r.GetLabelByID(ctx, id)
	return labelFrom(l), mapErr(err)
}

func (q *queries) ListLabels(ctx context.Context, moduleID string) ([]store.Label, error) {
	rows, err := q.r.ListLabels(ctx, moduleID)
	return mapAll(rows, err, labelFrom)
}

func (q *queries) ArchiveLabel(ctx context.Context, moduleID, name string, archived bool) error {
	now := ts(time.Time{})
	var archivedAt sql.NullInt64
	if archived {
		archivedAt = nullTS(&now)
	}
	return existing(q.w.ArchiveLabel(ctx, db.ArchiveLabelParams{Archived: b2i(archived), ArchivedAt: archivedAt,
		UpdatedAt: now.UnixMicro(), ModuleID: moduleID, Name: name}))
}

// --- commit deps ---

func (q *queries) AddCommitDeps(ctx context.Context, deps []store.CommitDep) error {
	for _, d := range deps {
		if err := q.w.AddCommitDep(ctx, db.AddCommitDepParams(d)); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

func (q *queries) ListCommitDeps(ctx context.Context, commitID string) ([]store.CommitDep, error) {
	rows, err := q.r.ListCommitDeps(ctx, commitID)
	return mapAll(rows, err, func(d db.CommitDep) store.CommitDep { return store.CommitDep(d) })
}

// --- blobs and manifests ---

func (q *queries) PutBlob(ctx context.Context, b store.Blob) error {
	size := b.Size
	if size == 0 {
		size = int64(len(b.Data))
	}
	data := b.Data
	if data == nil {
		data = []byte{} // the column is NOT NULL; contents may live in the blob store
	}
	return mapErr(q.w.PutBlob(ctx, db.PutBlobParams{Digest: b.Digest, Size: size, Data: data}))
}

func (q *queries) GetBlob(ctx context.Context, digest string) (store.Blob, error) {
	b, err := q.r.GetBlob(ctx, digest)
	return store.Blob(b), mapErr(err)
}

// SetManifest replaces the commit's manifest. Use it inside WithTx (or via
// Store.SetManifest) for atomicity.
func (q *queries) SetManifest(ctx context.Context, commitID string, entries []store.ManifestEntry) error {
	if err := q.w.DeleteManifest(ctx, commitID); err != nil {
		return mapErr(err)
	}
	for _, e := range entries {
		if err := q.w.AddManifestEntry(ctx, db.AddManifestEntryParams{CommitID: commitID, Path: e.Path,
			BlobDigest: e.BlobDigest}); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

func (q *queries) GetManifest(ctx context.Context, commitID string) ([]store.ManifestEntry, error) {
	rows, err := q.r.GetManifest(ctx, commitID)
	return mapAll(rows, err, func(e db.ManifestEntry) store.ManifestEntry { return store.ManifestEntry(e) })
}

func (q *queries) ListBlobDigests(ctx context.Context) ([]string, error) {
	rows, err := q.r.ListBlobDigests(ctx)
	return rows, mapErr(err)
}

func (q *queries) ListReferencedBlobDigests(ctx context.Context) ([]string, error) {
	rows, err := q.r.ListReferencedBlobDigests(ctx)
	return rows, mapErr(err)
}

// BlobReferenced reads through the writer so a transaction sees its own
// (and, outside one, all committed) manifest writes.
func (q *queries) BlobReferenced(ctx context.Context, digest string) (bool, error) {
	n, err := q.w.CountBlobRefs(ctx, digest)
	return n > 0, mapErr(err)
}

func (q *queries) DeleteBlob(ctx context.Context, digest string) error {
	n, err := q.w.DeleteUnreferencedBlob(ctx, digest)
	if err != nil || n > 0 {
		return mapErr(err)
	}
	// Nothing deleted: either the row is missing (fine) or still referenced.
	ref, err := q.BlobReferenced(ctx, digest)
	if err != nil {
		return err
	}
	if ref {
		return fmt.Errorf("%w: blob %s is still referenced", store.ErrConflict, digest)
	}
	return nil
}

// --- tokens ---

func tokenFrom(t db.Token) store.Token {
	return store.Token{ID: t.ID, OwnerID: t.OwnerID, Name: t.Name, Hash: t.Hash,
		CreatedAt: fromTS(t.CreatedAt), ExpiresAt: ptrTS(t.ExpiresAt), LastUsedAt: ptrTS(t.LastUsedAt),
		ModuleID: t.ModuleID.String, ModuleRole: t.ModuleRole}
}

func (q *queries) CreateToken(ctx context.Context, t store.Token) (store.Token, error) {
	if t.ID == "" {
		t.ID = newID()
	}
	t.CreatedAt = ts(t.CreatedAt)
	err := q.w.CreateToken(ctx, db.CreateTokenParams{ID: t.ID, OwnerID: t.OwnerID, Name: t.Name, Hash: t.Hash,
		CreatedAt: t.CreatedAt.UnixMicro(), ExpiresAt: nullTS(t.ExpiresAt), LastUsedAt: nullTS(t.LastUsedAt),
		ModuleID: sql.NullString{String: t.ModuleID, Valid: t.ModuleID != ""}, ModuleRole: t.ModuleRole})
	return t, mapErr(err)
}

func (q *queries) GetTokenByHash(ctx context.Context, hash string) (store.Token, error) {
	t, err := q.r.GetTokenByHash(ctx, hash)
	return tokenFrom(t), mapErr(err)
}

func (q *queries) ListTokens(ctx context.Context, ownerID string) ([]store.Token, error) {
	rows, err := q.r.ListTokens(ctx, ownerID)
	return mapAll(rows, err, tokenFrom)
}

func (q *queries) TouchToken(ctx context.Context, id string, at time.Time) error {
	t := ts(at)
	return existing(q.w.TouchToken(ctx, db.TouchTokenParams{LastUsedAt: nullTS(&t), ID: id}))
}

func (q *queries) DeleteToken(ctx context.Context, id string) error {
	return existing(q.w.DeleteToken(ctx, id))
}

func (q *queries) DeleteExpiredScopedTokens(ctx context.Context, now time.Time) (int64, error) {
	n, err := q.w.DeleteExpiredScopedTokens(ctx, nullTS(&now))
	return n, mapErr(err)
}

// --- trust credentials ---

func trustFrom(c db.TrustCredential) store.TrustCredential {
	return store.TrustCredential{ID: c.ID, Name: c.Name, ModuleID: c.ModuleID, UserID: c.UserID,
		Provider: c.Provider, Issuer: c.Issuer, Audience: c.Audience, SubjectPattern: c.SubjectPattern,
		Repository: c.Repository, RefPattern: c.RefPattern, Role: c.Role,
		TokenTTL: time.Duration(c.TokenTtlSeconds) * time.Second, CreatedAt: fromTS(c.CreatedAt)}
}

func (q *queries) CreateTrustCredential(ctx context.Context, c store.TrustCredential) (store.TrustCredential, error) {
	if c.ID == "" {
		c.ID = newID()
	}
	if c.TokenTTL > 0 && c.TokenTTL < time.Second {
		// Stored in whole seconds, it would become 0: the server default.
		return c, fmt.Errorf("%w: token TTL %s is below 1s", store.ErrConflict, c.TokenTTL)
	}
	c.CreatedAt = ts(c.CreatedAt)
	c.TokenTTL = c.TokenTTL.Truncate(time.Second)
	err := q.w.CreateTrustCredential(ctx, db.CreateTrustCredentialParams{ID: c.ID, Name: c.Name, ModuleID: c.ModuleID,
		UserID: c.UserID, Provider: c.Provider, Issuer: c.Issuer, Audience: c.Audience,
		SubjectPattern: c.SubjectPattern, Repository: c.Repository, RefPattern: c.RefPattern, Role: c.Role,
		TokenTtlSeconds: int64(c.TokenTTL / time.Second), CreatedAt: c.CreatedAt.UnixMicro()})
	return c, mapErr(err)
}

func (q *queries) GetTrustCredential(ctx context.Context, name string) (store.TrustCredential, error) {
	c, err := q.r.GetTrustCredential(ctx, name)
	return trustFrom(c), mapErr(err)
}

func (q *queries) ListTrustCredentials(ctx context.Context) ([]store.TrustCredential, error) {
	rows, err := q.r.ListTrustCredentials(ctx)
	return mapAll(rows, err, trustFrom)
}

func (q *queries) ListModuleTrustCredentials(ctx context.Context, moduleID string) ([]store.TrustCredential, error) {
	rows, err := q.r.ListModuleTrustCredentials(ctx, moduleID)
	return mapAll(rows, err, trustFrom)
}

func (q *queries) DeleteTrustCredential(ctx context.Context, name string) error {
	return existing(q.w.DeleteTrustCredential(ctx, name))
}

// --- identities ---

func identityFrom(i db.Identity) store.Identity {
	return store.Identity{Issuer: i.Issuer, Subject: i.Subject, OwnerID: i.OwnerID, Email: i.Email, Role: i.Role,
		CreatedAt: fromTS(i.CreatedAt), LastLoginAt: fromTS(i.LastLoginAt)}
}

func (q *queries) CreateIdentity(ctx context.Context, i store.Identity) (store.Identity, error) {
	i.CreatedAt = ts(i.CreatedAt)
	i.LastLoginAt = ts(i.LastLoginAt)
	err := q.w.CreateIdentity(ctx, db.CreateIdentityParams{Issuer: i.Issuer, Subject: i.Subject, OwnerID: i.OwnerID,
		Email: i.Email, Role: i.Role, CreatedAt: i.CreatedAt.UnixMicro(), LastLoginAt: i.LastLoginAt.UnixMicro()})
	return i, mapErr(err)
}

func (q *queries) GetIdentity(ctx context.Context, issuer, subject string) (store.Identity, error) {
	i, err := q.r.GetIdentity(ctx, db.GetIdentityParams{Issuer: issuer, Subject: subject})
	return identityFrom(i), mapErr(err)
}

func (q *queries) UpdateIdentity(ctx context.Context, i store.Identity) error {
	return existing(q.w.UpdateIdentity(ctx, db.UpdateIdentityParams{Email: i.Email, Role: i.Role,
		LastLoginAt: ts(i.LastLoginAt).UnixMicro(), Issuer: i.Issuer, Subject: i.Subject}))
}
