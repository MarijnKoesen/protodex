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
	"github.com/marijnkoesen/protodex/internal/config"
)

// Handler builds the HTTP handler: all MVP services (currently Unimplemented),
// plus /healthz and /readyz. Request bodies are capped at maxRequestBytes.
func Handler(log *slog.Logger, maxRequestBytes int64) http.Handler {
	opts := []connect.HandlerOption{
		connect.WithReadMaxBytes(int(min(maxRequestBytes, int64(^uint(0)>>1)))),
		// Panics in handlers or interceptors become Connect internal errors.
		recoveryOption(log),
		// Outermost first: log, authenticate, then validate the request message.
		connect.WithInterceptors(
			loggingInterceptor(log),
			authInterceptor(),
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
			return iamv1connect.NewTokenServiceHandler(iamv1connect.UnimplementedTokenServiceHandler{}, opts...)
		},
	} {
		path, h := reg()
		mux.Handle(path, h)
	}

	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	}
	mux.HandleFunc("GET /healthz", ok)
	// No dependencies yet; readiness will gain a database check later.
	mux.HandleFunc("GET /readyz", ok)

	return http.MaxBytesHandler(mux, maxRequestBytes)
}

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
func Serve(ctx context.Context, cfg config.Config, log *slog.Logger, ln net.Listener) error {
	srv := NewHTTPServer(cfg, Handler(log, cfg.MaxRequestBytes))

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
