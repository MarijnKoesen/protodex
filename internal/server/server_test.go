// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/server"
)

func start(t *testing.T, cfg config.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- server.Serve(ctx, cfg, log, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return "http://" + ln.Addr().String()
}

func TestHealth(t *testing.T) {
	base := start(t, config.Default())
	for _, p := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(base + p) //nolint:gosec,noctx // test
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", p, resp.StatusCode)
		}
	}
}

func TestUnimplemented(t *testing.T) {
	base := start(t, config.Default())
	client := modulev1connect.NewModuleServiceClient(http.DefaultClient, base)
	// A request that passes protovalidate reaches the stub handler.
	_, err := client.GetModules(context.Background(), connect.NewRequest(&modulev1.GetModulesRequest{
		ModuleRefs: []*modulev1.ModuleRef{{Value: &modulev1.ModuleRef_Name_{
			Name: &modulev1.ModuleRef_Name{Owner: "acme", Module: "weather"},
		}}},
	}))
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("code = %v (err %v), want unimplemented", got, err)
	}
}

func TestValidationRejectsInvalid(t *testing.T) {
	base := start(t, config.Default())
	client := modulev1connect.NewModuleServiceClient(http.DefaultClient, base)
	_, err := client.GetModules(context.Background(), connect.NewRequest(&modulev1.GetModulesRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("code = %v (err %v), want invalid_argument", got, err)
	}
}

func TestRequestSizeLimit(t *testing.T) {
	cfg := config.Default()
	cfg.MaxRequestBytes = 64
	base := start(t, cfg)
	body := strings.NewReader(`{"moduleRefs":[{"name":{"owner":"` + strings.Repeat("a", 512) + `","module":"m"}}]}`)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		base+modulev1connect.ModuleServiceGetModulesProcedure, body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("oversized request accepted")
	}
}

func TestShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, config.Default(), slog.New(slog.NewTextHandler(io.Discard, nil)), ln)
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown timed out")
	}
}
