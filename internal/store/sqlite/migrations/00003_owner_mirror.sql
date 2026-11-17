-- +goose Up
-- Mirror owners reserve an upstream owner name (e.g. googleapis) so a local
-- owner cannot shadow it. Only organizations can be mirrors.
ALTER TABLE owners ADD COLUMN mirror INTEGER NOT NULL DEFAULT 0
    CHECK (mirror IN (0, 1) AND (mirror = 0 OR type = 'organization'));
ALTER TABLE owners ADD COLUMN updated_at BIGINT NOT NULL DEFAULT 0;
UPDATE owners SET updated_at = created_at;

-- +goose Down
ALTER TABLE owners DROP COLUMN updated_at;
ALTER TABLE owners DROP COLUMN mirror;
