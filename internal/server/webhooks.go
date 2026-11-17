// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/marijnkoesen/protodex/gen/protodex/audit/v1"
	"github.com/marijnkoesen/protodex/gen/protodex/audit/v1/auditv1connect"
	webhookv1 "github.com/marijnkoesen/protodex/gen/protodex/webhook/v1"
	"github.com/marijnkoesen/protodex/gen/protodex/webhook/v1/webhookv1connect"
	"github.com/marijnkoesen/protodex/internal/audit"
	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/store"
)

const (
	defaultWebhookPage = 50
	maxWebhookPage     = 200
	defaultAuditPage   = 100
	maxAuditPage       = 500
	maxWebhooks        = 100
)

// webhookService implements protodex.webhook.v1.WebhookService (admins only).
type webhookService struct {
	webhookv1connect.UnimplementedWebhookServiceHandler
	st store.Store
}

func webhookProto(w store.Webhook) *webhookv1.Webhook {
	return &webhookv1.Webhook{Id: w.ID, Url: w.URL, Description: w.Description, EventTypes: w.EventTypes,
		Active: w.Active, CreateTime: timestamppb.New(w.CreatedAt), UpdateTime: timestamppb.New(w.UpdatedAt)}
}

// validateWebhookURL accepts absolute http(s) URLs without credentials.
func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.Hostname() == "":
		return invalidArg("webhook url must be an absolute http(s) URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return invalidArg("webhook url must use http or https")
	case u.User != nil:
		return invalidArg("webhook url must not contain credentials")
	}
	return nil
}

func validateEventTypes(types []string) ([]string, error) {
	out := make([]string, 0, len(types))
	for _, t := range types {
		if !audit.ValidPattern(t) {
			return nil, invalidArg("invalid event type pattern %q (want an exact type, a prefix like \"protodex.label.*\" or \"*\")", t)
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	if slices.Contains(out, "*") {
		out = nil // all events
	}
	return out, nil
}

func (s *webhookService) CreateWebhook(ctx context.Context, req *connect.Request[webhookv1.CreateWebhookRequest]) (*connect.Response[webhookv1.CreateWebhookResponse], error) {
	if _, err := caller(ctx); err != nil {
		return nil, err
	}
	m := req.Msg
	if err := validateWebhookURL(m.GetUrl()); err != nil {
		return nil, err
	}
	types, err := validateEventTypes(m.GetEventTypes())
	if err != nil {
		return nil, err
	}
	secret := m.GetSecret()
	if secret == "" {
		if secret, err = auth.GenerateSecret(); err != nil {
			return nil, internalErr(err)
		}
	}
	var w store.Webhook
	err = s.st.WithTx(ctx, func(q store.Querier) error {
		existing, err := q.ListWebhooks(ctx)
		if err != nil {
			return err
		}
		if len(existing) >= maxWebhooks {
			return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("at most %d webhooks are supported", maxWebhooks))
		}
		if w, err = q.CreateWebhook(ctx, store.Webhook{URL: m.GetUrl(), Secret: secret,
			Description: m.GetDescription(), EventTypes: types, Active: true}); err != nil {
			return err
		}
		_, err = audit.Record(ctx, q, audit.Event{Type: audit.WebhookCreated, Subject: w.ID,
			Data: map[string]any{"url": w.URL, "event_types": w.EventTypes}})
		return err
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&webhookv1.CreateWebhookResponse{Webhook: webhookProto(w), Secret: secret}), nil
}

func (s *webhookService) ListWebhooks(ctx context.Context, _ *connect.Request[webhookv1.ListWebhooksRequest]) (*connect.Response[webhookv1.ListWebhooksResponse], error) {
	ws, err := s.st.ListWebhooks(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	resp := &webhookv1.ListWebhooksResponse{}
	for _, w := range ws {
		resp.Webhooks = append(resp.Webhooks, webhookProto(w))
	}
	return connect.NewResponse(resp), nil
}

func (s *webhookService) UpdateWebhook(ctx context.Context, req *connect.Request[webhookv1.UpdateWebhookRequest]) (*connect.Response[webhookv1.UpdateWebhookResponse], error) {
	m := req.Msg
	var out store.Webhook
	err := s.st.WithTx(ctx, func(q store.Querier) error {
		w, err := q.GetWebhook(ctx, m.GetId())
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("webhook %q not found", m.GetId()))
		}
		if err != nil {
			return err
		}
		if m.Url != nil {
			if err := validateWebhookURL(m.GetUrl()); err != nil {
				return err
			}
			w.URL = m.GetUrl()
		}
		if m.Description != nil {
			w.Description = m.GetDescription()
		}
		if m.Active != nil {
			w.Active = m.GetActive()
		}
		if m.GetUpdateEventTypes() {
			if w.EventTypes, err = validateEventTypes(m.GetEventTypes()); err != nil {
				return err
			}
		}
		if out, err = q.UpdateWebhook(ctx, w); err != nil {
			return err
		}
		_, err = audit.Record(ctx, q, audit.Event{Type: audit.WebhookUpdated, Subject: out.ID,
			Data: map[string]any{"url": out.URL, "event_types": out.EventTypes, "active": out.Active}})
		return err
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&webhookv1.UpdateWebhookResponse{Webhook: webhookProto(out)}), nil
}

func (s *webhookService) DeleteWebhook(ctx context.Context, req *connect.Request[webhookv1.DeleteWebhookRequest]) (*connect.Response[webhookv1.DeleteWebhookResponse], error) {
	id := req.Msg.GetId()
	err := s.st.WithTx(ctx, func(q store.Querier) error {
		err := q.DeleteWebhook(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("webhook %q not found", id))
		}
		if err != nil {
			return err
		}
		_, err = audit.Record(ctx, q, audit.Event{Type: audit.WebhookDeleted, Subject: id})
		return err
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&webhookv1.DeleteWebhookResponse{}), nil
}

func pageSize(requested int32, def, maxSize int) int {
	switch {
	case requested <= 0:
		return def
	case int(requested) > maxSize:
		return maxSize
	}
	return int(requested)
}

func (s *webhookService) ListDeliveries(ctx context.Context, req *connect.Request[webhookv1.ListDeliveriesRequest]) (*connect.Response[webhookv1.ListDeliveriesResponse], error) {
	m := req.Msg
	if _, err := s.st.GetWebhook(ctx, m.GetWebhookId()); errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("webhook %q not found", m.GetWebhookId()))
	} else if err != nil {
		return nil, internalErr(err)
	}
	size := pageSize(m.GetPageSize(), defaultWebhookPage, maxWebhookPage)
	ds, err := s.st.ListDeliveries(ctx, m.GetWebhookId(), m.GetPageToken(), size+1)
	if err != nil {
		return nil, internalErr(err)
	}
	resp := &webhookv1.ListDeliveriesResponse{}
	if len(ds) > size {
		ds = ds[:size]
		resp.NextPageToken = ds[size-1].ID
	}
	for _, d := range ds {
		resp.Deliveries = append(resp.Deliveries, &webhookv1.Delivery{Id: d.ID, WebhookId: d.WebhookID,
			EventId: d.EventID, Status: d.Status, Attempts: int32(d.Attempts), //nolint:gosec // small
			NextAttemptTime: timestamppb.New(d.NextAttemptAt), LastStatus: int32(d.LastStatus), //nolint:gosec // HTTP status
			LastError: strings.TrimSpace(d.LastError), CreateTime: timestamppb.New(d.CreatedAt),
			UpdateTime: timestamppb.New(d.UpdatedAt)})
	}
	return connect.NewResponse(resp), nil
}

// auditService implements protodex.audit.v1.AuditService (admins only).
type auditService struct {
	auditv1connect.UnimplementedAuditServiceHandler
	st store.Store
}

func (s *auditService) ListEvents(ctx context.Context, req *connect.Request[auditv1.ListEventsRequest]) (*connect.Response[auditv1.ListEventsResponse], error) {
	m := req.Msg
	size := pageSize(m.GetPageSize(), defaultAuditPage, maxAuditPage)
	evs, err := s.st.ListAuditEvents(ctx, m.GetPageToken(), m.GetType(), size+1)
	if err != nil {
		return nil, internalErr(err)
	}
	resp := &auditv1.ListEventsResponse{}
	if len(evs) > size {
		evs = evs[:size]
		resp.NextPageToken = evs[size-1].ID
	}
	for _, e := range evs {
		resp.Events = append(resp.Events, &auditv1.Event{Id: e.ID, Type: e.Type, ActorId: e.ActorID,
			ActorName: e.ActorName, Subject: e.Subject, DataJson: e.Data, CreateTime: timestamppb.New(e.CreatedAt)})
	}
	return connect.NewResponse(resp), nil
}
