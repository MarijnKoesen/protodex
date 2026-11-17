-- name: PutMirrorCommit :exec
INSERT INTO mirror_commits (commit_id, upstream_url, upstream_ref, upstream_commit, license, imported_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: GetMirrorCommit :one
SELECT * FROM mirror_commits WHERE commit_id = ?;

-- name: ListMirrorCommits :many
SELECT m.* FROM mirror_commits m JOIN commits c ON c.id = m.commit_id
WHERE c.module_id = ? ORDER BY c.created_at DESC, c.id DESC;
