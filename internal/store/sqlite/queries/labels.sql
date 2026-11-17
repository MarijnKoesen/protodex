-- name: SetLabel :exec
INSERT INTO labels (id, module_id, name, commit_id, archived, archived_at, updated_by, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (module_id, name) DO UPDATE SET
  commit_id = excluded.commit_id,
  archived = excluded.archived,
  archived_at = excluded.archived_at,
  updated_by = excluded.updated_by,
  updated_at = excluded.updated_at;

-- name: GetLabel :one
SELECT * FROM labels WHERE module_id = ? AND name = ?;

-- name: GetLabelByID :one
SELECT * FROM labels WHERE id = ?;

-- name: ListLabels :many
SELECT * FROM labels WHERE module_id = ? ORDER BY name;

-- name: ArchiveLabel :execrows
UPDATE labels SET archived = ?, archived_at = ?, updated_at = ? WHERE module_id = ? AND name = ?;
