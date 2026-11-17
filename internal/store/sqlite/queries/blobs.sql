-- name: PutBlob :exec
INSERT INTO blobs (digest, size, data) VALUES (?, ?, ?)
ON CONFLICT (digest) DO NOTHING;

-- name: GetBlob :one
SELECT * FROM blobs WHERE digest = ?;

-- name: DeleteManifest :exec
DELETE FROM manifest_entries WHERE commit_id = ?;

-- name: AddManifestEntry :exec
INSERT INTO manifest_entries (commit_id, path, blob_digest) VALUES (?, ?, ?);

-- name: GetManifest :many
SELECT * FROM manifest_entries WHERE commit_id = ? ORDER BY path;

-- name: ListBlobDigests :many
SELECT digest FROM blobs ORDER BY digest;

-- name: ListReferencedBlobDigests :many
SELECT DISTINCT blob_digest FROM manifest_entries ORDER BY blob_digest;

-- name: CountBlobRefs :one
SELECT COUNT(*) FROM (SELECT 1 FROM manifest_entries WHERE blob_digest = ? LIMIT 1) AS r;

-- name: DeleteUnreferencedBlob :execrows
DELETE FROM blobs WHERE digest = ?
AND NOT EXISTS (SELECT 1 FROM manifest_entries m WHERE m.blob_digest = blobs.digest);
