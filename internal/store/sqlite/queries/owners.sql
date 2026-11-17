-- name: CreateOwner :exec
INSERT INTO owners (id, name, type, mirror, role, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetOwner :one
SELECT * FROM owners WHERE name = ?;

-- name: GetOwnerByID :one
SELECT * FROM owners WHERE id = ?;

-- name: ListOwners :many
SELECT * FROM owners ORDER BY name;

-- name: RenameOwner :execrows
UPDATE owners SET name = ?, updated_at = ? WHERE id = ? AND mirror = 0;

-- name: SetOwnerRole :execrows
UPDATE owners SET role = ?, updated_at = ? WHERE id = ? AND type = 'user';
