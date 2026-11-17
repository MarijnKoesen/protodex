// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesShellAndAssets(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux)
	for path, want := range map[string]string{
		"/":             "<title>Protodex</title>",
		"/ui/app.js":    "export",
		"/ui/style.css": "prefers-color-scheme",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: code %d, body lacks %q", path, rec.Code, want)
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("%s: missing CSP", path)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != 404 {
		t.Errorf("/nope: code %d, want 404", rec.Code)
	}
}
