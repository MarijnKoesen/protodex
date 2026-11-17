-- +goose Up
-- CI trust credentials (docs/ci-oidc.md): rules under which a workload
-- identity token (GitHub Actions OIDC, Kubernetes service account) is
-- exchanged for a short-lived registry token acting as user_id, limited to
-- one module and at most role.
CREATE TABLE trust_credentials (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,
    module_id         TEXT NOT NULL REFERENCES modules (id) ON DELETE CASCADE,
    user_id           TEXT NOT NULL REFERENCES owners (id) ON DELETE CASCADE,
    provider          TEXT NOT NULL CHECK (provider IN ('github', 'kubernetes')),
    issuer            TEXT NOT NULL,
    audience          TEXT NOT NULL,
    subject_pattern   TEXT NOT NULL DEFAULT '',
    repository        TEXT NOT NULL DEFAULT '',
    ref_pattern       TEXT NOT NULL DEFAULT '',
    role              TEXT NOT NULL CHECK (role IN ('read', 'write')),
    token_ttl_seconds BIGINT NOT NULL DEFAULT 0,
    created_at        BIGINT NOT NULL
);
CREATE INDEX trust_credentials_module_idx ON trust_credentials (module_id);

-- Tokens minted by a trust credential are restricted to one module and at
-- most module_role (read or write). Unscoped tokens leave both empty.
ALTER TABLE tokens ADD COLUMN module_id TEXT REFERENCES modules (id) ON DELETE CASCADE;
ALTER TABLE tokens ADD COLUMN module_role TEXT NOT NULL DEFAULT ''
    CHECK (module_role IN ('', 'read', 'write'));

-- +goose Down
ALTER TABLE tokens DROP COLUMN module_role;
ALTER TABLE tokens DROP COLUMN module_id;
DROP TABLE trust_credentials;
