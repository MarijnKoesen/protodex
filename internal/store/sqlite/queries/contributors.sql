-- name: SetModuleContributor :exec
INSERT INTO module_contributors (module_id, user_id, role, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (module_id, user_id) DO UPDATE SET
  role = excluded.role,
  updated_at = excluded.updated_at;

-- name: GetModuleContributor :one
SELECT * FROM module_contributors WHERE module_id = ? AND user_id = ?;

-- name: ListModuleContributors :many
SELECT * FROM module_contributors WHERE module_id = ? ORDER BY user_id;

-- name: ListUserContributions :many
SELECT * FROM module_contributors WHERE user_id = ? ORDER BY module_id;

-- name: DeleteModuleContributor :execrows
DELETE FROM module_contributors WHERE module_id = ? AND user_id = ?;
