-- +goose Up
-- Instance roles of users (viewer < member < admin; organizations have none)
-- and per-module contributor grants. Existing users become members, which
-- keeps the access they had before roles were enforced. The store never
-- writes an empty role for a user.
ALTER TABLE owners ADD COLUMN role TEXT NOT NULL DEFAULT ''
    CHECK (role IN ('', 'viewer', 'member', 'admin') AND (role = '' OR type = 'user'));
UPDATE owners SET role = 'member' WHERE type = 'user';

CREATE TABLE module_contributors (
    module_id  TEXT NOT NULL REFERENCES modules (id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES owners (id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('read', 'write', 'admin')),
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (module_id, user_id)
);
CREATE INDEX module_contributors_user_idx ON module_contributors (user_id);

-- +goose Down
DROP TABLE module_contributors;
ALTER TABLE owners DROP COLUMN role;
