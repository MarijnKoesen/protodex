// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/marijnkoesen/protodex/internal/observability"
)

// recoveryOption converts handler panics into Connect internal errors.
func recoveryOption(log *slog.Logger) connect.HandlerOption {
	return connect.WithRecover(func(ctx context.Context, spec connect.Spec, _ http.Header, panicValue any) error {
		log.ErrorContext(ctx, "panic in handler",
			"procedure", spec.Procedure, "panic", fmt.Sprint(panicValue), "stack", string(debug.Stack()))
		return connect.NewError(connect.CodeInternal, fmt.Errorf("internal error"))
	})
}

// observeInterceptor records metrics and a trace span for each unary RPC and
// reports the outcome to the access log via accessInfo.
func observeInterceptor(obs *observability.Observability) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			procedure := req.Spec().Procedure
			ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(req.Header()))
			ctx, span := obs.Tracer().Start(ctx, procedure,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("rpc.system", "connect_rpc"),
					attribute.String("rpc.method", procedure),
					attribute.String("protodex.request_id", observability.RequestID(ctx)),
				))
			defer span.End()

			start := time.Now()
			code := "ok"
			record := func() {
				obs.Metrics.ObserveRPC(procedure, code, time.Since(start))
				if info, ok := ctx.Value(accessKey{}).(*accessInfo); ok {
					info.procedure, info.code = procedure, code
				}
			}
			// Panics are converted to internal errors further out (recoveryOption);
			// record them here, then re-panic so that conversion still happens.
			defer func() {
				if p := recover(); p != nil {
					code = connect.CodeInternal.String()
					span.SetStatus(codes.Error, code)
					record()
					panic(p)
				}
			}()
			resp, err := next(ctx, req)
			if err != nil {
				c := connect.CodeOf(err)
				code = c.String()
				span.SetAttributes(attribute.String("rpc.connect_rpc.error_code", code))
				if c == connect.CodeInternal || c == connect.CodeUnknown {
					span.SetStatus(codes.Error, code)
				}
			}
			record()
			return resp, err
		}
	}
}

// authInterceptor is a placeholder: it currently lets every request through.
// Token authentication replaces it in a later change.
func authInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			return next(ctx, req)
		}
	}
}
