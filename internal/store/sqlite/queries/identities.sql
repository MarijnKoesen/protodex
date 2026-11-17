-- name: CreateIdentity :exec
INSERT INTO identities (issuer, subject, owner_id, email, role, created_at, last_login_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetIdentity :one
SELECT * FROM identities WHERE issuer = ? AND subject = ?;

-- name: UpdateIdentity :execrows
UPDATE identities SET email = ?, role = ?, last_login_at = ? WHERE issuer = ? AND subject = ?;
