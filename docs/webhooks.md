# Webhooks and audit log

Protodex keeps an append-only **audit log** of security-relevant changes and
can push every event to **webhooks** as [CloudEvents](https://cloudevents.io)
(structured JSON). Both are Protodex-native services (not part of the buf
registry API), defined in `proto/protodex/audit/v1` and
`proto/protodex/webhook/v1`, and are **instance-admin only**. Requires `db_path`; run
`protodex migrate` after upgrading (migration 8).

## Events

| Type | When |
|------|------|
| `protodex.push.created` | a push created a new commit |
| `protodex.label.updated` / `.archived` / `.unarchived` | a label moved (also by a push) or was archived |
| `protodex.module.created` / `.updated` / `.deleted` | module lifecycle and settings |
| `protodex.token.created` / `.deleted` | API tokens (also OIDC logins and the CLI); never the secret |
| `protodex.permission.role_changed` | a user's instance role changed (CLI, OIDC login) |
| `protodex.permission.contributor_granted` / `.contributor_revoked` | module contributor grants |
| `protodex.webhook.created` / `.updated` / `.deleted` | webhook management |

An event is written in the same database transaction as the change it
describes, so it exists exactly when the change was committed. The actor is
the authenticated user; changes made with the `protodex` CLI have no actor.

Read the log with `AuditService.ListEvents` (oldest first, filter by exact
`type`, page with `page_token`).

## Webhooks

`WebhookService` has `CreateWebhook`, `ListWebhooks`, `UpdateWebhook` (pause
with `active=false`, change subscriptions), `DeleteWebhook` and
`ListDeliveries` (state of each attempt). `event_types` selects events by
exact type or prefix (`protodex.label.*`); empty means all. URLs must be
`http(s)` without embedded credentials. The signing secret is generated on
creation and returned **once**.

```sh
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"url":"https://ci.example.com/hooks/protodex","eventTypes":["protodex.push.*"]}' \
  https://registry.example.com/protodex.webhook.v1.WebhookService/CreateWebhook
```

### Delivery

Matching events are queued in the database (durable across restarts) when
they are recorded. A background worker POSTs them:

```
Content-Type: application/cloudevents+json
X-Protodex-Signature: sha256=<hex HMAC-SHA256 of the body, keyed with the secret>
X-Protodex-Delivery: <delivery id>
```

```json
{"specversion":"1.0","id":"…","source":"protodex","type":"protodex.push.created",
 "subject":"acme/petapis","time":"2026-01-01T00:00:00Z","datacontenttype":"application/json",
 "protodexactor":"alice","data":{"module_id":"…","commit_id":"…","digest":"b5:…","files":12}}
```

Any 2xx response is success. Otherwise (other status, timeout, connection
error) the delivery is retried with exponential backoff (10s, 20s, 40s … capped
at 1h) and marked `failed` after `webhook_max_attempts`. Redirects are not
followed. Delivery is **at-least-once**: de-duplicate on the CloudEvent `id`
and verify the signature with a constant-time comparison.

Webhook URLs are chosen by admins and requested from the server, so the
worker can reach anything the server can; keep the admin role tight and use
network policy to restrict egress where that matters.

### Settings

| Setting | Env | Default | |
|---------|-----|---------|-|
| `webhook_interval` | `PROTODEX_WEBHOOK_INTERVAL` | `5s` | poll interval; `0s` disables delivery (events are still recorded and queued) |
| `webhook_timeout` | `PROTODEX_WEBHOOK_TIMEOUT` | `10s` | per attempt |
| `webhook_max_attempts` | | `8` | attempts before `failed` |

## Not included yet

A Kubernetes Events or generic CloudEvents-broker sink, audit-log retention/pruning,
and a web UI for the log.
