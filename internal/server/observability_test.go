// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/observability"
	"github.com/marijnkoesen/protodex/internal/server"
)

func TestMetricsTracingAndAccessLog(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	rec := tracetest.NewSpanRecorder()
	obs := observability.New().WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	ts := httptest.NewServer(server.Handler(log, 1<<20, obs))
	defer ts.Close()

	client := modulev1connect.NewModuleServiceClient(http.DefaultClient, ts.URL)
	_, err := client.GetModules(context.Background(), connect.NewRequest(&modulev1.GetModulesRequest{
		ModuleRefs: []*modulev1.ModuleRef{{Value: &modulev1.ModuleRef_Name_{
			Name: &modulev1.ModuleRef_Name{Owner: "acme", Module: "weather"},
		}}},
	}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("err = %v", err)
	}

	// Metrics are emitted and exposed on /metrics.
	resp, err := http.Get(ts.URL + "/metrics") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`protodex_rpc_requests_total{code="unimplemented",procedure="` + modulev1connect.ModuleServiceGetModulesProcedure + `"} 1`,
		"protodex_rpc_duration_seconds_bucket",
		"protodex_upload_bytes_total",
	} {
		if !strings.Contains(body.String(), want) {
			t.Errorf("/metrics missing %q", want)
		}
	}

	// Store and blob helpers record latency and spans.
	_ = obs.Store(context.Background(), "GetCommit", func(context.Context) error { return nil })
	_ = obs.Blob(context.Background(), "Put", func(context.Context) error { return context.Canceled })
	if n, err := testutil.GatherAndCount(obs.Metrics.Registry(),
		"protodex_store_operation_duration_seconds", "protodex_blob_operation_duration_seconds"); err != nil || n != 2 {
		t.Errorf("store/blob series = %d, err %v", n, err)
	}

	names := map[string]bool{}
	for _, s := range rec.Ended() {
		names[s.Name()] = true
	}
	for _, want := range []string{modulev1connect.ModuleServiceGetModulesProcedure, "protodex.store.GetCommit", "protodex.blob.Put"} {
		if !names[want] {
			t.Errorf("missing span %q (have %v)", want, names)
		}
	}

	// Access log has a request ID and the RPC outcome.
	if out := logs.String(); !strings.Contains(out, `"request_id"`) || !strings.Contains(out, `"code":"unimplemented"`) {
		t.Errorf("access log incomplete: %s", out)
	}
}

func TestRequestIDHandling(t *testing.T) {
	obs := observability.New()
	ts := httptest.NewServer(server.Handler(slog.New(slog.DiscardHandler), 1<<20, obs))
	defer ts.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/healthz", nil)
	req.Header.Set("X-Request-Id", "abc-123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != "abc-123" {
		t.Errorf("id = %q", got)
	}

	req.Header.Set("X-Request-Id", "bad id;forged")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got == "" || strings.ContainsAny(got, " ;") {
		t.Errorf("unsafe id not replaced: %q", got)
	}
}

func TestAccessLogRedactsAndPropagates(t *testing.T) {
	var logs bytes.Buffer
	rec := tracetest.NewSpanRecorder()
	obs := observability.New().WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	ts := httptest.NewServer(server.Handler(slog.New(slog.NewJSONHandler(&logs, nil)), 1<<20, obs))
	defer ts.Close()

	// Unknown path with secret, query and Authorization header.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/token/SECRETPATH?k=SECRETQUERY", nil)
	req.Header.Set("Authorization", "Bearer SECRETTOKEN")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// Upload bytes + traceparent propagation + request ID on span.
	const tid = "0af7651916cd43dd8448eb211c80319c"
	body := strings.NewReader(`{"contents":[]}`)
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+modulev1connect.UploadServiceUploadProcedure, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "req-1")
	req.Header.Set("Traceparent", "00-"+tid+"-b7ad6b7169203331-01")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	out := logs.String()
	for _, secret := range []string{"SECRETPATH", "SECRETQUERY", "SECRETTOKEN"} {
		if strings.Contains(out, secret) {
			t.Errorf("log leaks %s: %s", secret, out)
		}
	}
	want := fmt.Sprintf("protodex_upload_bytes_total %d", len(`{"contents":[]}`))
	mresp, err := http.Get(ts.URL + "/metrics") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var mb bytes.Buffer
	_, _ = mb.ReadFrom(mresp.Body)
	_ = mresp.Body.Close()
	if !strings.Contains(mb.String(), want) {
		t.Errorf("metrics missing %q", want)
	}
	found := false
	for _, s := range rec.Ended() {
		if s.SpanContext().TraceID().String() == tid {
			found = true
			for _, a := range s.Attributes() {
				if a.Key == "protodex.request_id" && a.Value.AsString() != "req-1" {
					t.Errorf("request id attr = %v", a.Value)
				}
			}
		}
	}
	if !found {
		t.Error("span not joined to incoming trace")
	}
}
