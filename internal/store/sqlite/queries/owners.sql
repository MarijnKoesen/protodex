-- name: CreateOwner :exec
INSERT INTO owners (id, name, type, mirror, role, description, url, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

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

-- name: DeleteOwner :execrows
DELETE FROM owners WHERE id = ?;

-- name: SetOwnerProfile :execrows
UPDATE owners SET description = ?, url = ?, updated_at = ? WHERE id = ?;
