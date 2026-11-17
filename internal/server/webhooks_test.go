// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	iamv1 "github.com/marijnkoesen/protodex/gen/buf/registry/iam/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/iam/v1/iamv1connect"
	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	auditv1 "github.com/marijnkoesen/protodex/gen/protodex/audit/v1"
	"github.com/marijnkoesen/protodex/gen/protodex/audit/v1/auditv1connect"
	webhookv1 "github.com/marijnkoesen/protodex/gen/protodex/webhook/v1"
	"github.com/marijnkoesen/protodex/gen/protodex/webhook/v1/webhookv1connect"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/webhook"
)

func TestWebhooksAndAuditLog(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	_, viewerSecret := userWithRole(t, e.st, "vera", store.RoleMember)

	hooks := webhookv1connect.NewWebhookServiceClient(http.DefaultClient, e.url, bearer(e.secret))
	events := auditv1connect.NewAuditServiceClient(http.DefaultClient, e.url, bearer(e.secret))

	// Only instance admins may manage webhooks or read the audit log.
	member := webhookv1connect.NewWebhookServiceClient(http.DefaultClient, e.url, bearer(viewerSecret))
	_, err := member.ListWebhooks(ctx, connect.NewRequest(&webhookv1.ListWebhooksRequest{}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = auditv1connect.NewAuditServiceClient(http.DefaultClient, e.url, bearer(viewerSecret)).
		ListEvents(ctx, connect.NewRequest(&auditv1.ListEventsRequest{}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = webhookv1connect.NewWebhookServiceClient(http.DefaultClient, e.url).
		ListWebhooks(ctx, connect.NewRequest(&webhookv1.ListWebhooksRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)

	// Validation.
	for _, bad := range []*webhookv1.CreateWebhookRequest{
		{Url: "ftp://example.com/x"},
		{Url: "not a url"},
		{Url: "https://user:pw@example.com/x"}, //nolint:gosec // test input
		{Url: "https://example.com/x", EventTypes: []string{"push.*.x"}},
	} {
		_, err := hooks.CreateWebhook(ctx, connect.NewRequest(bad))
		wantCode(t, err, connect.CodeInvalidArgument)
	}

	// A receiver that records what it is sent.
	var mu sync.Mutex
	var got []webhook.CloudEvent
	var sigOK []bool
	var secret string
	rcv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ce webhook.CloudEvent
		_ = json.Unmarshal(body, &ce)
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ce)
		sigOK = append(sigOK, webhook.Verify(secret, body, r.Header.Get(webhook.SignatureHeader)))
	}))
	t.Cleanup(rcv.Close)

	created, err := hooks.CreateWebhook(ctx, connect.NewRequest(&webhookv1.CreateWebhookRequest{
		Url: rcv.URL, EventTypes: []string{"protodex.module.*", "protodex.token.created"}}))
	if err != nil || created.Msg.GetSecret() == "" {
		t.Fatalf("CreateWebhook = %v, %v", created, err)
	}
	secret = created.Msg.GetSecret()
	id := created.Msg.GetWebhook().GetId()

	// Management events go to subscribers: they must not carry the URL's path or query.
	secretURL, err := hooks.CreateWebhook(ctx, connect.NewRequest(&webhookv1.CreateWebhookRequest{
		Url: rcv.URL + "/hook/s3cr3t?token=s3cr3t", EventTypes: []string{"protodex.webhook.*"}}))
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := events.ListEvents(ctx, connect.NewRequest(&auditv1.ListEventsRequest{}))
	for _, ev := range evs.Msg.GetEvents() {
		if strings.Contains(ev.GetDataJson(), "s3cr3t") {
			t.Fatalf("event leaks webhook URL: %s", ev.GetDataJson())
		}
	}
	if _, err := hooks.DeleteWebhook(ctx, connect.NewRequest(&webhookv1.DeleteWebhookRequest{Id: secretURL.Msg.GetWebhook().GetId()})); err != nil {
		t.Fatal(err)
	}

	// Changes made over the API are audited and queued.
	mods := modulev1connect.NewModuleServiceClient(http.DefaultClient, e.url, bearer(e.secret))
	createModule(t, mods, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("admin"), Name: "weather"})
	tok, err := iamv1connect.NewTokenServiceClient(http.DefaultClient, e.url, bearer(e.secret)).CreateTokens(ctx,
		connect.NewRequest(&iamv1.CreateTokensRequest{Values: []*iamv1.CreateTokensRequest_Value{{Name: "ci-token"}}}))
	if err != nil {
		t.Fatal(err)
	}

	list, err := events.ListEvents(ctx, connect.NewRequest(&auditv1.ListEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, ev := range list.Msg.GetEvents() {
		types = append(types, ev.GetType())
		if strings.Contains(ev.GetDataJson(), tok.Msg.GetValues()[0].GetSecret()) || strings.Contains(ev.GetDataJson(), secret) {
			t.Fatalf("event leaks a secret: %s", ev.GetDataJson())
		}
		if ev.GetActorName() != "admin" {
			t.Fatalf("actor = %q", ev.GetActorName())
		}
	}
	want := []string{"protodex.webhook.created", "protodex.webhook.created", "protodex.webhook.deleted", "protodex.module.created", "protodex.token.created"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want %v", types, want)
	}

	// Filter and paging.
	one, err := events.ListEvents(ctx, connect.NewRequest(&auditv1.ListEventsRequest{PageSize: 1}))
	if err != nil || len(one.Msg.GetEvents()) != 1 || one.Msg.GetNextPageToken() == "" {
		t.Fatalf("page 1 = %v, %v", one, err)
	}
	rest, err := events.ListEvents(ctx, connect.NewRequest(&auditv1.ListEventsRequest{PageToken: one.Msg.GetNextPageToken(), Type: "protodex.token.created"}))
	if err != nil || len(rest.Msg.GetEvents()) != 1 {
		t.Fatalf("filtered page = %v, %v", rest, err)
	}

	// Only the subscribed events (module created, token created) were queued.
	ds, err := hooks.ListDeliveries(ctx, connect.NewRequest(&webhookv1.ListDeliveriesRequest{WebhookId: id}))
	if err != nil || len(ds.Msg.GetDeliveries()) != 2 || ds.Msg.GetDeliveries()[0].GetStatus() != store.DeliveryPending {
		t.Fatalf("deliveries = %v, %v", ds, err)
	}

	// The worker delivers signed CloudEvents.
	n, err := webhook.New(e.st, webhook.Options{}).RunOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	mu.Lock()
	if len(got) != 2 || got[0].Type != "protodex.module.created" || got[0].SpecVersion != "1.0" || got[0].Subject != "admin/weather" || got[1].Type != "protodex.token.created" {
		t.Fatalf("received %+v", got)
	}
	if !sigOK[0] || !sigOK[1] {
		t.Fatal("signature mismatch")
	}
	mu.Unlock()
	ds, _ = hooks.ListDeliveries(ctx, connect.NewRequest(&webhookv1.ListDeliveriesRequest{WebhookId: id}))
	for _, d := range ds.Msg.GetDeliveries() {
		if d.GetStatus() != store.DeliveryDelivered || d.GetAttempts() != 1 || d.GetLastStatus() != 200 {
			t.Fatalf("delivery = %v", d)
		}
	}

	// Pausing stops queuing; deleting removes the webhook and its deliveries.
	if _, err := hooks.UpdateWebhook(ctx, connect.NewRequest(&webhookv1.UpdateWebhookRequest{Id: id, Active: new(false)})); err != nil {
		t.Fatal(err)
	}
	createModule(t, mods, &modulev1.CreateModulesRequest_Value{OwnerRef: ownerName("admin"), Name: "other"})
	ds, _ = hooks.ListDeliveries(ctx, connect.NewRequest(&webhookv1.ListDeliveriesRequest{WebhookId: id}))
	if len(ds.Msg.GetDeliveries()) != 2 {
		t.Fatalf("paused webhook got new deliveries: %v", ds)
	}
	if _, err := hooks.DeleteWebhook(ctx, connect.NewRequest(&webhookv1.DeleteWebhookRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	_, err = hooks.ListDeliveries(ctx, connect.NewRequest(&webhookv1.ListDeliveriesRequest{WebhookId: id}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = hooks.DeleteWebhook(ctx, connect.NewRequest(&webhookv1.DeleteWebhookRequest{Id: id}))
	wantCode(t, err, connect.CodeNotFound)
}
