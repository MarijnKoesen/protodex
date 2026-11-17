// SPDX-License-Identifier: Apache-2.0

// Package webhook delivers audit events to subscribers as CloudEvents
// (structured JSON mode) over HTTP POST.
//
// Delivery is durable: audit.Record queues a row per subscriber in the same
// transaction as the change; the Worker claims due rows, POSTs them and, on
// failure, reschedules with exponential backoff until MaxAttempts is
// exhausted. Delivery is at-least-once: receivers must de-duplicate on the
// CloudEvent id.
//
// Each request carries
//
//	X-Protodex-Signature: sha256=<hex HMAC-SHA256(secret, body)>
//
// so receivers can verify authenticity; compare with hmac.Equal.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/marijnkoesen/protodex/internal/store"
)

// SignatureHeader carries the HMAC of the request body.
const SignatureHeader = "X-Protodex-Signature"

// Sign returns the signature header value for body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Verify reports whether signature is the valid signature of body.
func Verify(secret string, body []byte, signature string) bool {
	return hmac.Equal([]byte(Sign(secret, body)), []byte(signature))
}

// CloudEvent is the CloudEvents 1.0 JSON envelope of an audit event.
type CloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject,omitempty"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Actor           string          `json:"protodexactor,omitempty"`
	Data            json.RawMessage `json:"data"`
}

// Source is the CloudEvents source attribute.
const Source = "protodex"

// ToCloudEvent converts an audit event.
func ToCloudEvent(e store.AuditEvent) CloudEvent {
	data := json.RawMessage(e.Data)
	if !json.Valid(data) {
		data = json.RawMessage("{}")
	}
	return CloudEvent{SpecVersion: "1.0", ID: e.ID, Source: Source, Type: e.Type, Subject: e.Subject,
		Time: e.CreatedAt.UTC().Format(time.RFC3339Nano), DataContentType: "application/json",
		Actor: e.ActorName, Data: data}
}

// Options configure a Worker.
type Options struct {
	// Client sends the requests; nil selects a client with Timeout.
	Client *http.Client
	// Timeout per attempt (default 10s).
	Timeout time.Duration
	// MaxAttempts before a delivery is marked failed (default 8).
	MaxAttempts int
	// BaseBackoff is the delay after the first failure; it doubles per
	// attempt up to MaxBackoff (defaults 10s and 1h).
	BaseBackoff, MaxBackoff time.Duration
	// BatchSize bounds the deliveries handled per poll (default 20).
	BatchSize int
	Logger    *slog.Logger
	Now       func() time.Time
}

// Worker sends queued deliveries.
type Worker struct {
	st   store.Store
	opts Options
}

// New returns a Worker.
func New(st store.Store, o Options) *Worker {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: o.Timeout,
			// A redirect could point at an internal address the admin never configured.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 8
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 10 * time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = time.Hour
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 20
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Worker{st: st, opts: o}
}

// Run polls every interval until ctx is cancelled.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.opts.Logger.Error("webhook delivery run", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce attempts every due delivery once and returns how many were handled.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	due, err := w.st.ListDueDeliveries(ctx, w.opts.Now(), w.opts.BatchSize)
	if err != nil {
		return 0, err
	}
	for _, d := range due {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := w.deliver(ctx, d); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}

func (w *Worker) deliver(ctx context.Context, d store.Delivery) error {
	hook, err := w.st.GetWebhook(ctx, d.WebhookID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // deleted meanwhile; the cascade removes the row
	}
	if err != nil {
		return err
	}
	if !hook.Active {
		// Paused: keep the delivery and look again later.
		d.NextAttemptAt = w.opts.Now().Add(w.opts.BaseBackoff)
		return w.st.UpdateDelivery(ctx, d)
	}
	ev, err := w.st.GetAuditEvent(ctx, d.EventID)
	if err != nil {
		return err
	}
	status, sendErr := w.send(ctx, hook, ev, d)
	d.Attempts++
	d.LastStatus = status
	d.LastError = ""
	switch {
	case sendErr == nil:
		d.Status = store.DeliveryDelivered
	case d.Attempts >= w.opts.MaxAttempts:
		d.Status = store.DeliveryFailed
		d.LastError = sendErr.Error()
	default:
		d.LastError = sendErr.Error()
		d.NextAttemptAt = w.opts.Now().Add(w.backoff(d.Attempts))
	}
	if sendErr != nil {
		w.opts.Logger.Warn("webhook delivery failed", "webhook", hook.ID, "event", ev.ID,
			"attempt", d.Attempts, "status", d.Status, "error", sendErr)
	}
	return w.st.UpdateDelivery(ctx, d)
}

// backoff is BaseBackoff * 2^(attempts-1), capped at MaxBackoff.
func (w *Worker) backoff(attempts int) time.Duration {
	d := w.opts.BaseBackoff
	for i := 1; i < attempts && d < w.opts.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, w.opts.MaxBackoff)
}

func (w *Worker) send(ctx context.Context, hook store.Webhook, ev store.AuditEvent, d store.Delivery) (int, error) {
	body, err := json.Marshal(ToCloudEvent(ev))
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, w.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/cloudevents+json; charset=utf-8")
	req.Header.Set("User-Agent", "protodex-webhook")
	req.Header.Set(SignatureHeader, Sign(hook.Secret, body))
	req.Header.Set("X-Protodex-Delivery", d.ID)
	resp, err := w.opts.Client.Do(req)
	if err != nil {
		// *url.Error embeds the request URL, which may carry credentials in
		// its path or query: keep only the underlying cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("receiver returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}
