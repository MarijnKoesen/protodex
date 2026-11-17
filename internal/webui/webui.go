// SPDX-License-Identifier: Apache-2.0

// Package webui embeds the single-page web UI. The SPA is plain ES modules
// (no build step) that talks only to the public Connect API using the Connect
// JSON protocol.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var assets embed.FS

// Register mounts the UI on mux: the app shell at "/" and its assets under
// "/ui/". The SPA uses hash routing so no other path needs to be claimed.
func Register(mux *http.ServeMux) {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err) // embedded path is fixed at compile time
	}
	files := http.FileServerFS(sub)
	mux.Handle("GET /{$}", secure(files))
	mux.Handle("GET /ui/", secure(http.StripPrefix("/ui/", files)))
}

// secure sets a strict CSP (no inline script, same-origin API calls only).
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
