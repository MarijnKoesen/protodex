-- name: CreateModule :exec
INSERT INTO modules (id, owner_id, name, visibility, default_label, description, url, deprecated, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetModule :one
SELECT * FROM modules WHERE owner_id = ? AND name = ?;

-- name: GetModuleByID :one
SELECT * FROM modules WHERE id = ?;

-- name: ListModules :many
SELECT * FROM modules WHERE owner_id = ? ORDER BY name;

-- name: ListAllModules :many
SELECT * FROM modules ORDER BY owner_id, name;

-- name: UpdateModule :execrows
UPDATE modules SET visibility = ?, default_label = ?, description = ?, url = ?, deprecated = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteModuleLabels :exec
DELETE FROM labels WHERE module_id = ?;

-- name: DeleteModule :execrows
DELETE FROM modules WHERE id = ?;
