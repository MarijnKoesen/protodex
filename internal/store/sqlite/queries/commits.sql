-- name: CreateCommit :exec
INSERT INTO commits (id, module_id, digest, created_at, source_control_url) VALUES (?, ?, ?, ?, ?);

-- name: GetCommit :one
SELECT * FROM commits WHERE module_id = ? AND id = ?;

-- name: ListCommits :many
SELECT * FROM commits WHERE module_id = ? ORDER BY created_at DESC, id DESC;

-- name: AddCommitDep :exec
INSERT INTO commit_deps (commit_id, dep_registry, dep_owner, dep_module, dep_commit_id, dep_digest)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListCommitDeps :many
SELECT * FROM commit_deps WHERE commit_id = ?
ORDER BY dep_registry, dep_owner, dep_module;

-- name: GetCommitByID :one
SELECT * FROM commits WHERE id = ?;
