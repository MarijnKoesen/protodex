-- +goose Up
-- Module settings exposed by ModuleService, and label identity/timestamps
-- exposed by LabelService. Label ids are backfilled by migration 5 (Go),
-- which also adds their unique index.
ALTER TABLE modules ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE modules ADD COLUMN url TEXT NOT NULL DEFAULT '';
ALTER TABLE modules ADD COLUMN deprecated INTEGER NOT NULL DEFAULT 0 CHECK (deprecated IN (0, 1));
ALTER TABLE modules ADD COLUMN updated_at BIGINT NOT NULL DEFAULT 0;
UPDATE modules SET updated_at = created_at;

ALTER TABLE labels ADD COLUMN id TEXT NOT NULL DEFAULT '';
ALTER TABLE labels ADD COLUMN created_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE labels ADD COLUMN archived_at BIGINT;
-- owner id of the user that last moved the label; '' when unknown
ALTER TABLE labels ADD COLUMN updated_by TEXT NOT NULL DEFAULT '';
UPDATE labels SET created_at = updated_at;
UPDATE labels SET archived_at = updated_at WHERE archived = 1;

-- +goose Down
ALTER TABLE labels DROP COLUMN updated_by;
ALTER TABLE labels DROP COLUMN archived_at;
ALTER TABLE labels DROP COLUMN created_at;
ALTER TABLE labels DROP COLUMN id;
ALTER TABLE modules DROP COLUMN updated_at;
ALTER TABLE modules DROP COLUMN deprecated;
ALTER TABLE modules DROP COLUMN url;
ALTER TABLE modules DROP COLUMN description;
