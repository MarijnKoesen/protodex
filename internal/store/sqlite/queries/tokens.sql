-- name: CreateToken :exec
INSERT INTO tokens (id, owner_id, name, hash, created_at, expires_at, last_used_at, module_id, module_role)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetTokenByHash :one
SELECT * FROM tokens WHERE hash = ?;

-- name: ListTokens :many
SELECT * FROM tokens WHERE owner_id = ? ORDER BY created_at, id;

-- name: TouchToken :execrows
UPDATE tokens SET last_used_at = ? WHERE id = ?;

-- name: DeleteToken :execrows
DELETE FROM tokens WHERE id = ?;

-- name: DeleteExpiredScopedTokens :execrows
DELETE FROM tokens WHERE module_id IS NOT NULL AND expires_at IS NOT NULL AND expires_at <= ?;
