-- name: CreateAuditEvent :exec
INSERT INTO audit_events (id, type, actor_id, actor_name, subject, data, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetAuditEvent :one
SELECT * FROM audit_events WHERE id = ?;

-- name: ListAuditEvents :many
SELECT * FROM audit_events
WHERE id > sqlc.arg(after) AND (sqlc.arg(type) = '' OR type = sqlc.arg(type))
ORDER BY id
LIMIT sqlc.arg(max);

-- name: CreateWebhook :exec
INSERT INTO webhooks (id, url, secret, description, event_types, active, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetWebhook :one
SELECT * FROM webhooks WHERE id = ?;

-- name: ListWebhooks :many
SELECT * FROM webhooks ORDER BY created_at, id;

-- name: UpdateWebhook :execrows
UPDATE webhooks SET url = ?, description = ?, event_types = ?, active = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteWebhook :execrows
DELETE FROM webhooks WHERE id = ?;

-- name: CreateDelivery :exec
INSERT INTO webhook_deliveries (id, webhook_id, event_id, status, attempts, next_attempt_at,
    last_status, last_error, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListDueDeliveries :many
SELECT * FROM webhook_deliveries
WHERE status = 'pending' AND next_attempt_at <= ?
ORDER BY next_attempt_at, id
LIMIT ?;

-- name: ListDeliveries :many
SELECT * FROM webhook_deliveries WHERE webhook_id = sqlc.arg(webhook_id) AND id > sqlc.arg(after)
ORDER BY id
LIMIT sqlc.arg(max);

-- name: UpdateDelivery :execrows
UPDATE webhook_deliveries
SET status = ?, attempts = ?, next_attempt_at = ?, last_status = ?, last_error = ?, updated_at = ?
WHERE id = ?;
