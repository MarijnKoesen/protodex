-- name: CreateTrustCredential :exec
INSERT INTO trust_credentials (id, name, module_id, user_id, provider, issuer, audience,
  subject_pattern, repository, ref_pattern, role, token_ttl_seconds, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetTrustCredential :one
SELECT * FROM trust_credentials WHERE name = ?;

-- name: ListTrustCredentials :many
SELECT * FROM trust_credentials ORDER BY name;

-- name: ListModuleTrustCredentials :many
SELECT * FROM trust_credentials WHERE module_id = ? ORDER BY name;

-- name: DeleteTrustCredential :execrows
DELETE FROM trust_credentials WHERE name = ?;
