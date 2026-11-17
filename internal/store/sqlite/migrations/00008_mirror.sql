-- +goose Up
-- Commits imported by the mirror keep their upstream provenance. The
-- source_control_url is served as Commit.source_control_url.
ALTER TABLE commits ADD COLUMN source_control_url TEXT NOT NULL DEFAULT '';

CREATE TABLE mirror_commits (
    commit_id       TEXT PRIMARY KEY REFERENCES commits (id) ON DELETE CASCADE,
    upstream_url    TEXT NOT NULL,
    upstream_ref    TEXT NOT NULL,
    upstream_commit TEXT NOT NULL,
    license         TEXT NOT NULL,
    imported_at     BIGINT NOT NULL
);

-- +goose Down
DROP TABLE mirror_commits;
ALTER TABLE commits DROP COLUMN source_control_url;
