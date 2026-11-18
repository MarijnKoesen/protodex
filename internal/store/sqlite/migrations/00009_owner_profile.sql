-- +goose Up
-- Description and URL of users and organizations, editable over the API
-- (`buf registry organization update`).
ALTER TABLE owners ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE owners ADD COLUMN url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE owners DROP COLUMN url;
ALTER TABLE owners DROP COLUMN description;
