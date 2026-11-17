// SPDX-License-Identifier: Apache-2.0

// Package observability provides Prometheus metrics, OpenTelemetry tracing
// and request-ID plumbing. Metric names are documented in docs/observability.md.
package observability

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/marijnkoesen/protodex"

// Metrics holds all Protodex collectors on a private registry.
type Metrics struct {
	reg *prometheus.Registry

	rpcTotal      *prometheus.CounterVec
	rpcDuration   *prometheus.HistogramVec
	uploadBytes   prometheus.Counter
	storeDuration *prometheus.HistogramVec
	blobDuration  *prometheus.HistogramVec
	httpInFlight  prometheus.Gauge

	gcRuns        *prometheus.CounterVec
	gcDuration    prometheus.Histogram
	gcLastSuccess prometheus.Gauge
	gcBlobs       *prometheus.CounterVec
	gcBytes       *prometheus.CounterVec
}

// NewMetrics creates a registry with Go/process collectors and Protodex metrics.
func NewMetrics() *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}
	m.rpcTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "protodex_rpc_requests_total",
		Help: "Completed RPCs by procedure and Connect code.",
	}, []string{"procedure", "code"})
	m.rpcDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "protodex_rpc_duration_seconds",
		Help:    "RPC latency by procedure and Connect code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"procedure", "code"})
	m.uploadBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "protodex_upload_bytes_total",
		Help: "Bytes of uploaded content accepted.",
	})
	m.storeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "protodex_store_operation_duration_seconds",
		Help:    "Metadata store operation latency.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation", "status"})
	m.blobDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "protodex_blob_operation_duration_seconds",
		Help:    "Blob storage operation latency.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation", "status"})
	m.httpInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "protodex_http_in_flight_requests",
		Help: "HTTP requests currently being served.",
	})
	m.gcRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "protodex_gc_runs_total",
		Help: "Blob garbage-collection runs by result (ok or error).",
	}, []string{"result"})
	m.gcDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "protodex_gc_duration_seconds",
		Help:    "Duration of blob garbage-collection runs.",
		Buckets: prometheus.ExponentialBuckets(0.01, 4, 10),
	})
	m.gcLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "protodex_gc_last_success_timestamp_seconds",
		Help: "Unix time of the last successful blob garbage-collection run.",
	})
	m.gcBlobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "protodex_gc_blobs_total",
		Help: "Unreferenced blobs handled by garbage collection, by action.",
	}, []string{"action"})
	m.gcBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "protodex_gc_bytes_total",
		Help: "Bytes of unreferenced blobs deleted (or, in dry-run mode, that would be).",
	}, []string{"action"})
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.rpcTotal, m.rpcDuration, m.uploadBytes, m.storeDuration, m.blobDuration, m.httpInFlight,
		m.gcRuns, m.gcDuration, m.gcLastSuccess, m.gcBlobs, m.gcBytes,
	)
	return m
}

// Handler serves the metrics in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry exposes the registry (used by tests).
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// ObserveRPC records a finished RPC.
func (m *Metrics) ObserveRPC(procedure, code string, d time.Duration) {
	m.rpcTotal.WithLabelValues(procedure, code).Inc()
	m.rpcDuration.WithLabelValues(procedure, code).Observe(d.Seconds())
}

// AddUploadBytes records accepted upload content size.
func (m *Metrics) AddUploadBytes(n int64) {
	if n > 0 {
		m.uploadBytes.Add(float64(n))
	}
}

// GC actions recorded by AddGCBlobs.
const (
	GCDeleted    = "deleted"    // unreferenced and removed
	GCDryRun     = "dry_run"    // would have been removed (dry-run mode)
	GCInFlight   = "in_flight"  // spared: pinned by an in-flight upload
	GCReferenced = "referenced" // spared: referenced again when re-checked
	GCFailed     = "failed"     // removal failed; retried next run
)

// ObserveGCRun records a finished garbage-collection run.
func (m *Metrics) ObserveGCRun(err error, d time.Duration) {
	result := "ok"
	if err != nil {
		result = "error"
	} else {
		m.gcLastSuccess.SetToCurrentTime()
	}
	m.gcRuns.WithLabelValues(result).Inc()
	m.gcDuration.Observe(d.Seconds())
}

// AddGCBlobs records n blobs (of size bytes in total) handled with action.
func (m *Metrics) AddGCBlobs(action string, n int, bytes int64) {
	if n > 0 {
		m.gcBlobs.WithLabelValues(action).Add(float64(n))
	}
	if bytes > 0 && (action == GCDeleted || action == GCDryRun) {
		m.gcBytes.WithLabelValues(action).Add(float64(bytes))
	}
}

// InFlight returns the in-flight HTTP gauge.
func (m *Metrics) InFlight() prometheus.Gauge { return m.httpInFlight }

// Store times and traces a metadata-store operation. Use it to wrap each
// store call: err := obs.Store(ctx, "GetCommit", func(ctx context.Context) error {...}).
func (o *Observability) Store(ctx context.Context, op string, fn func(context.Context) error) error {
	return o.timed(ctx, "store", op, o.Metrics.storeDuration, fn)
}

// Blob times and traces a blob-storage operation.
func (o *Observability) Blob(ctx context.Context, op string, fn func(context.Context) error) error {
	return o.timed(ctx, "blob", op, o.Metrics.blobDuration, fn)
}

func (o *Observability) timed(ctx context.Context, kind, op string, h *prometheus.HistogramVec, fn func(context.Context) error) error {
	ctx, span := o.Tracer().Start(ctx, "protodex."+kind+"."+op,
		trace.WithAttributes(attribute.String("protodex.operation", op)))
	defer span.End()
	start := time.Now()
	err := fn(ctx)
	status := "ok"
	if err != nil {
		status = "error"
		// Only the error type is exported: messages may contain secrets or content.
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
		span.SetStatus(codes.Error, "operation failed")
	}
	h.WithLabelValues(op, status).Observe(time.Since(start).Seconds())
	return err
}

// Observability bundles metrics and tracing for injection.
type Observability struct {
	Metrics *Metrics
	tp      trace.TracerProvider
}

// New returns an Observability using the global tracer provider at call time.
func New() *Observability {
	return &Observability{Metrics: NewMetrics()}
}

// WithTracerProvider overrides the tracer provider (tests, explicit wiring).
func (o *Observability) WithTracerProvider(tp trace.TracerProvider) *Observability {
	o.tp = tp
	return o
}

// Tracer returns the tracer to use.
func (o *Observability) Tracer() trace.Tracer {
	if o.tp != nil {
		return o.tp.Tracer(tracerName)
	}
	return otel.Tracer(tracerName)
}

type ctxKey struct{}

// WithRequestID stores a request ID in ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// RequestID returns the request ID stored in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
