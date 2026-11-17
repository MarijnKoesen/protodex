// SPDX-License-Identifier: Apache-2.0

// Package server wires the Connect handlers, interceptors and HTTP plumbing.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/validate"

	"github.com/marijnkoesen/protodex/gen/buf/registry/iam/v1/iamv1connect"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1/ownerv1connect"
	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/observability"
	"github.com/marijnkoesen/protodex/internal/oidcauth"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/version"
)

// Handler builds the HTTP handler: all MVP services (currently Unimplemented),
// plus /healthz, /readyz and /metrics. Request bodies are capped at maxRequestBytes.
//
// Without WithStore no authentication is performed and TokenService stays
// unimplemented.
func Handler(log *slog.Logger, maxRequestBytes int64, obs *observability.Observability, options ...Option) http.Handler {
	var o serverOptions
	for _, f := range options {
		f(&o)
	}
	var authn *auth.Authenticator
	if o.store != nil {
		authn = &auth.Authenticator{Store: o.store, Limiter: o.limiter}
	}
	opts := []connect.HandlerOption{
		connect.WithReadMaxBytes(int(min(maxRequestBytes, int64(^uint(0)>>1)))),
		// Panics in handlers or interceptors become Connect internal errors.
		recoveryOption(log),
		// Outermost first: observe, authenticate, then validate the request message.
		connect.WithInterceptors(
			observeInterceptor(obs),
			authInterceptor(authn),
			validate.NewInterceptor(),
		),
	}

	mux := http.NewServeMux()
	for _, reg := range []func() (string, http.Handler){
		func() (string, http.Handler) {
			return modulev1connect.NewCommitServiceHandler(modulev1connect.UnimplementedCommitServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			return modulev1connect.NewDownloadServiceHandler(modulev1connect.UnimplementedDownloadServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			return modulev1connect.NewFileDescriptorSetServiceHandler(modulev1connect.UnimplementedFileDescriptorSetServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			return modulev1connect.NewGraphServiceHandler(modulev1connect.UnimplementedGraphServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			return modulev1connect.NewLabelServiceHandler(modulev1connect.UnimplementedLabelServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			return modulev1connect.NewModuleServiceHandler(modulev1connect.UnimplementedModuleServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			return modulev1connect.NewResourceServiceHandler(modulev1connect.UnimplementedResourceServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			return modulev1connect.NewUploadServiceHandler(modulev1connect.UnimplementedUploadServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			return ownerv1connect.NewOrganizationServiceHandler(ownerv1connect.UnimplementedOrganizationServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			return ownerv1connect.NewOwnerServiceHandler(ownerv1connect.UnimplementedOwnerServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			return ownerv1connect.NewUserServiceHandler(ownerv1connect.UnimplementedUserServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			if o.store == nil {
				return iamv1connect.NewTokenServiceHandler(iamv1connect.UnimplementedTokenServiceHandler{}, opts...)
			}
			return iamv1connect.NewTokenServiceHandler(&tokenService{st: o.store}, opts...)
		},
	} {
		path, h := reg()
		mux.Handle(path, h)
	}

	if o.store != nil {
		path, h := authnAlphaHandler(opts...)
		mux.Handle(path, h)
	}

	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	}
	mux.HandleFunc("GET /healthz", ok)
	// No dependencies yet; readiness will gain a database check later.
	mux.HandleFunc("GET /readyz", ok)

	if o.oidc != nil {
		o.oidc.Register(mux)
	}

	mux.Handle("GET /metrics", obs.Metrics.Handler())

	return accessLog(log, obs, http.MaxBytesHandler(mux, maxRequestBytes))
}

type serverOptions struct {
	store   store.Store
	limiter auth.Limiter
	oidc    *oidcauth.Handler
}

// Option configures Handler and Serve.
type Option func(*serverOptions)

// WithStore enables token authentication and TokenService backed by st.
func WithStore(st store.Store) Option { return func(o *serverOptions) { o.store = st } }

// WithLimiter sets the authentication rate-limit hook.
func WithLimiter(l auth.Limiter) Option { return func(o *serverOptions) { o.limiter = l } }

// WithOIDC mounts the OIDC login endpoints (/auth/login, /auth/callback).
func WithOIDC(h *oidcauth.Handler) Option { return func(o *serverOptions) { o.oidc = h } }

// NewHTTPServer returns an http.Server serving h over HTTP/1.1 and cleartext
// HTTP/2 (h2c), with sensible timeouts.
func NewHTTPServer(cfg config.Config, h http.Handler) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		Protocols:         &protocols,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
}

// Serve serves on ln until ctx is cancelled, then shuts down gracefully
// within cfg.ShutdownTimeout.
func Serve(ctx context.Context, cfg config.Config, log *slog.Logger, ln net.Listener, options ...Option) error {
	shutdownTracing, err := observability.SetupTracing(ctx, observability.TracingConfig{
		Enabled:        cfg.TracingEnabled,
		Endpoint:       cfg.OTLPEndpoint,
		ServiceVersion: version.String(),
	})
	if err != nil {
		return err
	}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(tctx); err != nil {
			log.Warn("tracing shutdown", "error", err)
		}
	}()

	srv := NewHTTPServer(cfg, Handler(log, cfg.MaxRequestBytes, observability.New(), options...))

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownTimeout))
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
