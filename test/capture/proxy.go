// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"time"
)

// Proxy is a reverse proxy that forwards to Upstream and appends every
// exchange, sanitised, as a JSON line to Out.
type Proxy struct {
	Upstream   *url.URL
	Out        io.Writer
	CLIVersion string
	Scenario   string
	// Transport is used for upstream requests; nil means http.DefaultTransport.
	Transport http.RoundTripper
	// KeepBinary stores non-JSON bodies (best-effort scrubbed). Off by default.
	KeepBinary bool

	mu sync.Mutex
}

// Handler returns the http.Handler to serve.
func (p *Proxy) Handler() http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(p.Upstream)
			r.Out.Host = p.Upstream.Host
		},
		Transport: p.Transport,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body so we can record it, then restore it for forwarding.
		reqBytes, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(reqBytes))
		reqHeader := SanitizeHeaders(r.Header)
		reqCT := r.Header.Get("Content-Type")

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		rp.ServeHTTP(rec, r)

		svc, rpc := SplitRPC(r.URL.Path)
		p.write(Exchange{
			Time: time.Now().UTC(), CLIVersion: p.CLIVersion, Scenario: p.Scenario,
			Method: r.Method, Path: r.URL.Path, Service: svc, RPC: rpc,
			Status:     rec.status,
			ReqHeader:  reqHeader,
			RespHeader: SanitizeHeaders(rec.Header()),
			ReqBody:    NewBody(reqBytes, reqCT, p.KeepBinary),
			RespBody:   NewBody(rec.buf.Bytes(), rec.Header().Get("Content-Type"), p.KeepBinary),
		})
	})
}

func (p *Proxy) write(e Exchange) {
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.Out.Write(append(line, '\n'))
	if f, ok := p.Out.(*os.File); ok {
		_ = f.Sync()
	}
}

type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}

// Flush supports streaming responses.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
