// SPDX-License-Identifier: Apache-2.0

// Package audit records security-relevant events (pushes, label, token,
// module and permission changes) in an append-only log and queues them for
// delivery to webhooks.
//
// Record runs inside the caller's transaction, so an event exists exactly
// when the change it describes was committed, and its delivery rows are
// created atomically with it: nothing is lost if the process dies right
// after the commit. The webhook worker (package webhook) does the sending.
package audit

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/store"
)

// Event types. Webhooks subscribe to exact types or to prefixes like "protodex.label.*".
const (
	PushCreated        = "protodex.push.created"
	LabelUpdated       = "protodex.label.updated"
	LabelArchived      = "protodex.label.archived"
	LabelUnarchived    = "protodex.label.unarchived"
	ModuleCreated      = "protodex.module.created"
	ModuleUpdated      = "protodex.module.updated"
	ModuleDeleted      = "protodex.module.deleted"
	TokenCreated       = "protodex.token.created" //nolint:gosec // event type, not a credential
	TokenDeleted       = "protodex.token.deleted" //nolint:gosec // event type, not a credential
	UserRoleChanged    = "protodex.permission.role_changed"
	ContributorGranted = "protodex.permission.contributor_granted"
	ContributorRevoked = "protodex.permission.contributor_revoked"
	WebhookCreated     = "protodex.webhook.created"
	WebhookUpdated     = "protodex.webhook.updated"
	WebhookDeleted     = "protodex.webhook.deleted"
)

// Event is what a caller reports. Data must be JSON-encodable and must never
// contain secrets (token secrets, webhook secrets).
type Event struct {
	Type    string
	Subject string
	Data    map[string]any
	// Actor overrides the acting user taken from ctx (e.g. for the CLI,
	// where there is none; leave empty for "system").
	Actor *store.Owner
}

// Record appends e to the audit log and queues a delivery for every active
// webhook subscribed to its type. Call it inside the transaction that makes
// the change.
func Record(ctx context.Context, q store.Querier, e Event) (store.AuditEvent, error) {
	data := e.Data
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return store.AuditEvent{}, err
	}
	ev := store.AuditEvent{Type: e.Type, Subject: e.Subject, Data: string(raw)}
	switch {
	case e.Actor != nil:
		ev.ActorID, ev.ActorName = e.Actor.ID, e.Actor.Name
	default:
		if id, ok := auth.FromContext(ctx); ok {
			ev.ActorID, ev.ActorName = id.Owner.ID, id.Owner.Name
		}
	}
	ev, err = q.CreateAuditEvent(ctx, ev)
	if err != nil {
		return ev, err
	}
	hooks, err := q.ListWebhooks(ctx)
	if err != nil {
		return ev, err
	}
	for _, h := range hooks {
		if !h.Active || !Matches(h.EventTypes, ev.Type) {
			continue
		}
		if _, err := q.CreateDelivery(ctx, store.Delivery{WebhookID: h.ID, EventID: ev.ID,
			NextAttemptAt: time.Now()}); err != nil {
			return ev, err
		}
	}
	return ev, nil
}

// Matches reports whether an event type is selected by patterns: exact
// types, prefixes ending in ".*", or "*". No patterns select everything.
func Matches(patterns []string, eventType string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		switch {
		case p == "*" || p == eventType:
			return true
		case strings.HasSuffix(p, ".*") && strings.HasPrefix(eventType, strings.TrimSuffix(p, "*")):
			return true
		}
	}
	return false
}

// ValidPattern reports whether p is an acceptable subscription pattern.
func ValidPattern(p string) bool {
	if p == "" || strings.Contains(p, ",") || strings.ContainsAny(p, " \t\n") {
		return false
	}
	if i := strings.Index(p, "*"); i >= 0 {
		return p == "*" || (i == len(p)-1 && strings.HasSuffix(p, ".*"))
	}
	return true
}
