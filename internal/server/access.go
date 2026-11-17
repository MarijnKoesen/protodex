// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/marijnkoesen/protodex/internal/observability"
)

const requestIDHeader = "X-Request-Id"

// validRequestID restricts client-supplied IDs so they cannot inject log content.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type accessKey struct{}

// accessInfo is filled in by the RPC interceptor for the access log.
type accessInfo struct {
	procedure string
	code      string
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type countingBody struct {
	io.ReadCloser
	n int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

// accessLog assigns a request ID, counts upload bytes and writes one
// structured access-log line per request. It never logs headers, query
// strings or bodies, so tokens and file contents cannot leak.
func accessLog(log *slog.Logger, obs *observability.Observability, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)

		info := &accessInfo{}
		ctx := context.WithValue(observability.WithRequestID(r.Context(), id), accessKey{}, info)
		r = r.WithContext(ctx)

		var body *countingBody
		isUpload := strings.Contains(r.URL.Path, "UploadService/")
		if isUpload && r.Body != nil {
			body = &countingBody{ReadCloser: r.Body}
			r.Body = body
		}

		obs.Metrics.InFlight().Inc()
		defer obs.Metrics.InFlight().Dec()

		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		if body != nil {
			obs.Metrics.AddUploadBytes(body.n)
		}

		attrs := []any{
			"request_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"bytes_out", sw.bytes,
			"duration", time.Since(start),
			"remote", r.RemoteAddr,
		}
		if info.procedure != "" {
			attrs = append(attrs, "procedure", info.procedure, "code", info.code)
		}
		level := slog.LevelInfo
		if sw.status >= 500 {
			level = slog.LevelError
		}
		log.Log(ctx, level, "access", attrs...)
	})
}
