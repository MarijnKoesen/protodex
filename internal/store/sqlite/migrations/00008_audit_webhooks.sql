-- +goose Up
-- Audit log, webhooks and their durable delivery queue. Event ids sort by
-- creation time (see audit.NewID), so the id doubles as the paging cursor.
CREATE TABLE audit_events (
    id         TEXT PRIMARY KEY,
    type       TEXT NOT NULL,
    actor_id   TEXT NOT NULL DEFAULT '',
    actor_name TEXT NOT NULL DEFAULT '',
    subject    TEXT NOT NULL DEFAULT '',
    data       TEXT NOT NULL DEFAULT '{}',
    created_at BIGINT NOT NULL
);
CREATE INDEX audit_events_type_idx ON audit_events (type, id);

CREATE TABLE webhooks (
    id          TEXT PRIMARY KEY,
    url         TEXT NOT NULL,
    secret      TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    -- comma-separated event types or prefixes ("push.*"); empty means all
    event_types TEXT NOT NULL DEFAULT '',
    active      INTEGER NOT NULL DEFAULT 1,
    created_at  BIGINT NOT NULL,
    updated_at  BIGINT NOT NULL
);

CREATE TABLE webhook_deliveries (
    id              TEXT PRIMARY KEY,
    webhook_id      TEXT NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    event_id        TEXT NOT NULL REFERENCES audit_events (id) ON DELETE CASCADE,
    status          TEXT NOT NULL CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at BIGINT NOT NULL,
    last_status     INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL
);
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (status, next_attempt_at);
CREATE INDEX webhook_deliveries_webhook_idx ON webhook_deliveries (webhook_id, id);

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE webhooks;
DROP TABLE audit_events;
