-- name: SetLabel :exec
INSERT INTO labels (module_id, name, commit_id, archived, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (module_id, name) DO UPDATE SET
  commit_id = excluded.commit_id,
  archived = excluded.archived,
  updated_at = excluded.updated_at;

-- name: GetLabel :one
SELECT * FROM labels WHERE module_id = ? AND name = ?;

-- name: ListLabels :many
SELECT * FROM labels WHERE module_id = ? ORDER BY name;

-- name: ArchiveLabel :execrows
UPDATE labels SET archived = ?, updated_at = ? WHERE module_id = ? AND name = ?;
