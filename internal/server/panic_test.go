// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/marijnkoesen/protodex/internal/observability"
)

func TestPanicIsRecordedAsInternal(t *testing.T) {
	obs := observability.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := connect.NewUnaryHandler("/x.Svc/Boom",
		func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			panic("boom")
		},
		recoveryOption(log), connect.WithInterceptors(observeInterceptor(obs)))
	ts := httptest.NewServer(accessLog(log, obs, h2("/x.Svc/Boom", h)))
	defer ts.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/x.Svc/Boom", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	mrec := httptest.NewRecorder()
	obs.Metrics.Handler().ServeHTTP(mrec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	want := `protodex_rpc_requests_total{code="internal",procedure="/x.Svc/Boom"} 1`
	if !strings.Contains(mrec.Body.String(), want) {
		t.Errorf("panic not recorded; metrics:\n%s", mrec.Body.String())
	}
}

func h2(path string, h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(path, h)
	return mux
}
