-- +goose Up
-- Portable subset: no INSERT OR REPLACE, no AUTOINCREMENT, no SQLite-only
-- functions. Timestamps are unix microseconds (BIGINT); booleans are 0/1
-- INTEGER; blobs are BLOB (BYTEA on Postgres).
CREATE TABLE owners (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    type       TEXT NOT NULL CHECK (type IN ('user', 'organization')),
    created_at BIGINT NOT NULL
);

CREATE TABLE modules (
    id            TEXT PRIMARY KEY,
    owner_id      TEXT NOT NULL REFERENCES owners (id),
    name          TEXT NOT NULL,
    visibility    TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
    default_label TEXT NOT NULL,
    created_at    BIGINT NOT NULL,
    UNIQUE (owner_id, name)
);

CREATE TABLE commits (
    id         TEXT PRIMARY KEY,
    module_id  TEXT NOT NULL REFERENCES modules (id) ON DELETE CASCADE,
    digest     TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    UNIQUE (module_id, id)
);
CREATE INDEX commits_module_idx ON commits (module_id, created_at);

CREATE TABLE labels (
    module_id  TEXT NOT NULL REFERENCES modules (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    commit_id  TEXT NOT NULL,
    archived   INTEGER NOT NULL DEFAULT 0,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (module_id, name),
    -- a label may only point at a commit of its own module
    FOREIGN KEY (module_id, commit_id) REFERENCES commits (module_id, id)
);

CREATE TABLE commit_deps (
    commit_id     TEXT NOT NULL REFERENCES commits (id) ON DELETE CASCADE,
    dep_registry  TEXT NOT NULL DEFAULT '',
    dep_owner     TEXT NOT NULL,
    dep_module    TEXT NOT NULL,
    dep_commit_id TEXT NOT NULL,
    dep_digest    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (commit_id, dep_registry, dep_owner, dep_module)
);

CREATE TABLE blobs (
    digest TEXT PRIMARY KEY,
    size   BIGINT NOT NULL,
    data   BLOB NOT NULL
);

CREATE TABLE manifest_entries (
    commit_id   TEXT NOT NULL REFERENCES commits (id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    blob_digest TEXT NOT NULL REFERENCES blobs (digest),
    PRIMARY KEY (commit_id, path)
);

CREATE TABLE tokens (
    id           TEXT PRIMARY KEY,
    owner_id     TEXT NOT NULL REFERENCES owners (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    hash         TEXT NOT NULL UNIQUE,
    created_at   BIGINT NOT NULL,
    expires_at   BIGINT,
    last_used_at BIGINT
);

-- +goose Down
DROP TABLE tokens;
DROP TABLE manifest_entries;
DROP TABLE blobs;
DROP TABLE commit_deps;
DROP TABLE labels;
DROP TABLE commits;
DROP TABLE modules;
DROP TABLE owners;
