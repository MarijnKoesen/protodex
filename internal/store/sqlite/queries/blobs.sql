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
