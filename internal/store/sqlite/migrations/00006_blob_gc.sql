-- +goose Up
-- Blob garbage collection asks "does any manifest still reference this
-- blob?" for every candidate; index the reverse edge.
CREATE INDEX manifest_entries_blob_idx ON manifest_entries (blob_digest);

-- +goose Down
DROP INDEX manifest_entries_blob_idx;
