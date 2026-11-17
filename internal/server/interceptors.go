// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/marijnkoesen/protodex/internal/auth"
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

// authInterceptor resolves "Authorization: Bearer <token>" to an identity.
// Invalid, malformed or expired credentials are rejected as unauthenticated.
// Requests without credentials are anonymous: they pass only for procedures
// the policy table (permissions.go) marks anonymous; module visibility is
// enforced by the handlers. With a nil authenticator
// no authentication is performed.
func authInterceptor(a *auth.Authenticator) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if a == nil {
				return next(ctx, req)
			}
			header := req.Header().Get("Authorization")
			if header == "" {
				if !anonymousAllowed(req.Spec().Procedure) {
					return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("authentication required"))
				}
				return next(ctx, req)
			}
			scheme, secret, ok := strings.Cut(header, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid authorization header"))
			}
			id, err := a.Authenticate(ctx, strings.TrimSpace(secret), req.Peer().Addr)
			switch {
			case errors.Is(err, auth.ErrInvalidToken):
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or expired token"))
			case errors.Is(err, auth.ErrRateLimited):
				return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many authentication attempts"))
			case err != nil:
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
			return next(auth.WithIdentity(ctx, id), req)
		}
	}
}
