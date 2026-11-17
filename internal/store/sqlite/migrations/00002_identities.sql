-- +goose Up
-- External (OIDC) identities, keyed by the stable (issuer, subject) pair and
-- never by e-mail. role is re-derived from the IdP's claims on every login.
CREATE TABLE identities (
    issuer        TEXT NOT NULL,
    subject       TEXT NOT NULL,
    owner_id      TEXT NOT NULL REFERENCES owners (id) ON DELETE CASCADE,
    email         TEXT NOT NULL,
    role          TEXT NOT NULL,
    created_at    BIGINT NOT NULL,
    last_login_at BIGINT NOT NULL,
    PRIMARY KEY (issuer, subject)
);
CREATE INDEX identities_owner_idx ON identities (owner_id);

-- +goose Down
DROP TABLE identities;
