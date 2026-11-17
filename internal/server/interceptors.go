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
)

// recoveryOption converts handler panics into Connect internal errors.
func recoveryOption(log *slog.Logger) connect.HandlerOption {
	return connect.WithRecover(func(ctx context.Context, spec connect.Spec, _ http.Header, panicValue any) error {
		log.ErrorContext(ctx, "panic in handler",
			"procedure", spec.Procedure, "panic", fmt.Sprint(panicValue), "stack", string(debug.Stack()))
		return connect.NewError(connect.CodeInternal, fmt.Errorf("internal error"))
	})
}

// loggingInterceptor logs one line per unary RPC.
func loggingInterceptor(log *slog.Logger) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			start := time.Now()
			resp, err := next(ctx, req)
			attrs := []any{
				"procedure", req.Spec().Procedure,
				"duration", time.Since(start),
				"code", "ok",
			}
			level := slog.LevelInfo
			if err != nil {
				code := connect.CodeOf(err)
				attrs[5] = code.String()
				if code == connect.CodeInternal || code == connect.CodeUnknown {
					level = slog.LevelError
				}
			}
			log.Log(ctx, level, "rpc", attrs...)
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
