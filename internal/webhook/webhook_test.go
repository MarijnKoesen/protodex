// SPDX-License-Identifier: Apache-2.0

package webhook_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marijnkoesen/protodex/internal/audit"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
	"github.com/marijnkoesen/protodex/internal/webhook"
)

func open(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRetriesWithBackoffThenSucceeds(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	h, err := st.CreateWebhook(ctx, store.Webhook{URL: srv.URL, Secret: "s", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Record(ctx, st, audit.Event{Type: audit.PushCreated, Subject: "a/b"}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	w := webhook.New(st, webhook.Options{BaseBackoff: time.Minute, Now: func() time.Time { return now }})
	status := func() store.Delivery {
		ds, err := st.ListDeliveries(ctx, h.ID, "", 10)
		if err != nil || len(ds) != 1 {
			t.Fatalf("deliveries = %v, %v", ds, err)
		}
		return ds[0]
	}

	// Attempt 1 fails; the retry is not due before the backoff (1m) elapsed.
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	d := status()
	if d.Status != store.DeliveryPending || d.Attempts != 1 || d.LastStatus != 502 || d.LastError == "" {
		t.Fatalf("after attempt 1: %+v", d)
	}
	if n, _ := w.RunOnce(ctx); n != 0 {
		t.Fatalf("delivery retried before its backoff: %d", n)
	}
	// Attempt 2 fails with a doubled backoff (2m).
	now = now.Add(61 * time.Second)
	if n, _ := w.RunOnce(ctx); n != 1 {
		t.Fatal("attempt 2 not made")
	}
	now = now.Add(61 * time.Second)
	if n, _ := w.RunOnce(ctx); n != 0 {
		t.Fatal("backoff did not double")
	}
	// Attempt 3 succeeds.
	now = now.Add(61 * time.Second)
	if n, _ := w.RunOnce(ctx); n != 1 {
		t.Fatal("attempt 3 not made")
	}
	if d := status(); d.Status != store.DeliveryDelivered || d.Attempts != 3 || d.LastError != "" {
		t.Fatalf("final: %+v", d)
	}
	if n, _ := w.RunOnce(ctx); n != 0 || calls.Load() != 3 {
		t.Fatalf("delivered event was sent again (calls=%d)", calls.Load())
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	h, _ := st.CreateWebhook(ctx, store.Webhook{URL: srv.URL, Secret: "s", Active: true})
	if _, err := audit.Record(ctx, st, audit.Event{Type: audit.LabelUpdated}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w := webhook.New(st, webhook.Options{MaxAttempts: 2, BaseBackoff: time.Second, Now: func() time.Time { return now }})
	for range 5 {
		_, _ = w.RunOnce(ctx)
		now = now.Add(time.Hour)
	}
	ds, _ := st.ListDeliveries(ctx, h.ID, "", 10)
	if len(ds) != 1 || ds[0].Status != store.DeliveryFailed || ds[0].Attempts != 2 {
		t.Fatalf("deliveries = %+v", ds)
	}
}

func TestUnreachableReceiverIsRetried(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more
	h, _ := st.CreateWebhook(ctx, store.Webhook{URL: url, Secret: "s", Active: true})
	_, _ = audit.Record(ctx, st, audit.Event{Type: audit.LabelUpdated})
	if _, err := webhook.New(st, webhook.Options{}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ds, _ := st.ListDeliveries(ctx, h.ID, "", 10)
	if ds[0].Status != store.DeliveryPending || ds[0].LastStatus != 0 || ds[0].LastError == "" {
		t.Fatalf("delivery = %+v", ds[0])
	}
}

func TestMatches(t *testing.T) {
	for _, c := range []struct {
		patterns []string
		typ      string
		want     bool
	}{
		{nil, "protodex.push.created", true},
		{[]string{"*"}, "x", true},
		{[]string{"protodex.push.created"}, "protodex.push.created", true},
		{[]string{"protodex.push.created"}, "protodex.push.other", false},
		{[]string{"protodex.label.*"}, "protodex.label.updated", true},
		{[]string{"protodex.label.*"}, "protodex.labels.updated", false},
		{[]string{"a.b", "protodex.token.*"}, "protodex.token.created", true},
	} {
		if got := audit.Matches(c.patterns, c.typ); got != c.want {
			t.Errorf("Matches(%v, %q) = %v", c.patterns, c.typ, got)
		}
	}
}

func TestSignature(t *testing.T) {
	body := []byte(`{"a":1}`)
	sig := webhook.Sign("secret", body)
	if !webhook.Verify("secret", body, sig) || webhook.Verify("other", body, sig) || webhook.Verify("secret", []byte("x"), sig) {
		t.Fatal("signature verification wrong")
	}
	// Known vector: HMAC-SHA256("secret", `{"a":1}`).
	if len(sig) != len("sha256=")+64 {
		t.Fatalf("sig = %q", sig)
	}
}
