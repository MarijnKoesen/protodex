-- name: CreateOwner :exec
INSERT INTO owners (id, name, type, created_at) VALUES (?, ?, ?, ?);

-- name: GetOwner :one
SELECT * FROM owners WHERE name = ?;

-- name: ListOwners :many
SELECT * FROM owners ORDER BY name;
