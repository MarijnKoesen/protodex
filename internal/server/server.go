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
	"github.com/marijnkoesen/protodex/internal/blob"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/gc"
	"github.com/marijnkoesen/protodex/internal/observability"
	"github.com/marijnkoesen/protodex/internal/oidcauth"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/version"
)

// Handler builds the HTTP handler: all MVP services (currently Unimplemented),
// plus /healthz, /readyz and /metrics. Request bodies are capped at maxRequestBytes.
//
// Without WithStore no authentication is performed and the TokenService,
// owner, module, label, commit and upload services stay unimplemented
// (UploadService also needs WithBlobs).
func Handler(log *slog.Logger, maxRequestBytes int64, obs *observability.Observability, options ...Option) http.Handler {
	o := newServerOptions(options)
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
			if o.store == nil {
				return modulev1connect.NewCommitServiceHandler(modulev1connect.UnimplementedCommitServiceHandler{}, opts...)
			}
			return modulev1connect.NewCommitServiceHandler(&commitService{st: o.store}, opts...)
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
			if o.store == nil {
				return modulev1connect.NewLabelServiceHandler(modulev1connect.UnimplementedLabelServiceHandler{}, opts...)
			}
			return modulev1connect.NewLabelServiceHandler(&labelService{st: o.store}, opts...)
		},
		func() (string, http.Handler) {
			if o.store == nil {
				return modulev1connect.NewModuleServiceHandler(modulev1connect.UnimplementedModuleServiceHandler{}, opts...)
			}
			return modulev1connect.NewModuleServiceHandler(&moduleService{st: o.store}, opts...)
		},
		func() (string, http.Handler) {
			return modulev1connect.NewResourceServiceHandler(modulev1connect.UnimplementedResourceServiceHandler{}, opts...)
		},
		func() (string, http.Handler) {
			if o.store == nil || o.blobs == nil {
				return modulev1connect.NewUploadServiceHandler(modulev1connect.UnimplementedUploadServiceHandler{}, opts...)
			}
			return modulev1connect.NewUploadServiceHandler(&uploadService{st: o.store, blobs: o.blobs, obs: obs,
				limits: o.limits.withDefaults(), leases: o.leases}, opts...)
		},
		func() (string, http.Handler) {
			if o.store == nil {
				return ownerv1connect.NewOrganizationServiceHandler(ownerv1connect.UnimplementedOrganizationServiceHandler{}, opts...)
			}
			return ownerv1connect.NewOrganizationServiceHandler(&organizationService{st: o.store}, opts...)
		},
		func() (string, http.Handler) {
			if o.store == nil {
				return ownerv1connect.NewOwnerServiceHandler(ownerv1connect.UnimplementedOwnerServiceHandler{}, opts...)
			}
			return ownerv1connect.NewOwnerServiceHandler(&ownerService{st: o.store}, opts...)
		},
		func() (string, http.Handler) {
			if o.store == nil {
				return ownerv1connect.NewUserServiceHandler(ownerv1connect.UnimplementedUserServiceHandler{}, opts...)
			}
			return ownerv1connect.NewUserServiceHandler(&userService{st: o.store}, opts...)
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
	gc      GCConfig
	leases  gc.Leases
	blobs   blob.Store
	limits  UploadLimits
	store   store.Store
	limiter auth.Limiter
	oidc    *oidcauth.Handler
}

func newServerOptions(options []Option) serverOptions {
	var o serverOptions
	for _, f := range options {
		f(&o)
	}
	if o.leases == nil {
		o.leases = gc.NewPins()
	}
	return o
}

// Option configures Handler and Serve.
type Option func(*serverOptions)

// WithStore enables token authentication, TokenService and the owner,
// module and label services backed by st.
func WithStore(st store.Store) Option { return func(o *serverOptions) { o.store = st } }

// WithBlobs sets the blob store file contents are pushed to. UploadService
// needs both a store and a blob store; without them it stays unimplemented.
func WithBlobs(b blob.Store) Option { return func(o *serverOptions) { o.blobs = b } }

// WithBlobLeases sets the leases pushes pin their blobs with, so that a
// garbage collector sharing them never deletes a blob an in-flight push
// references. Default: a fresh in-process gc.Pins.
func WithBlobLeases(l gc.Leases) Option { return func(o *serverOptions) { o.leases = l } }

// GCConfig configures the in-process blob garbage collector.
type GCConfig struct {
	// Interval between runs; zero disables the collector.
	Interval time.Duration
	// Grace is the minimum blob age before deletion (zero: gc.DefaultGrace).
	Grace time.Duration
	// DryRun logs and counts what would be deleted without deleting.
	DryRun bool
}

// WithGC runs the blob garbage collector inside Serve (it needs WithStore
// and a listable WithBlobs). It shares its leases with the upload service.
func WithGC(c GCConfig) Option { return func(o *serverOptions) { o.gc = c } }

// WithUploadLimits bounds the files of a push.
func WithUploadLimits(l UploadLimits) Option { return func(o *serverOptions) { o.limits = l } }

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

	obs := observability.New()
	o := newServerOptions(options)
	// Handler and collector must share one set of leases.
	options = append(options, WithBlobLeases(o.leases))
	srv := NewHTTPServer(cfg, Handler(log, cfg.MaxRequestBytes, obs, options...))

	if o.gc.Interval > 0 && o.store != nil && o.blobs != nil {
		col, err := gc.New(o.store, o.blobs, o.leases, gc.Options{Grace: o.gc.Grace, DryRun: o.gc.DryRun,
			Metrics: obs.Metrics, Logger: log.With("component", "gc")})
		if err != nil {
			return err
		}
		gcCtx, stopGC := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); col.Run(gcCtx, o.gc.Interval, gc.SingleProcess{}) }()
		defer func() { stopGC(); <-done }()
		log.Info("blob gc enabled", "interval", o.gc.Interval, "grace", o.gc.Grace, "dry_run", o.gc.DryRun)
	}

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
