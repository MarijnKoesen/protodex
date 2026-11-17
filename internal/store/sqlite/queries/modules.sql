-- name: CreateModule :exec
INSERT INTO modules (id, owner_id, name, visibility, default_label, created_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: GetModule :one
SELECT * FROM modules WHERE owner_id = ? AND name = ?;

-- name: ListModules :many
SELECT * FROM modules WHERE owner_id = ? ORDER BY name;

-- name: DeleteModuleLabels :exec
DELETE FROM labels WHERE module_id = ?;

-- name: DeleteModule :execrows
DELETE FROM modules WHERE id = ?;
