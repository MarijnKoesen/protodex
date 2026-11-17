// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSplitRPC(t *testing.T) {
	s, m := SplitRPC("/buf.registry.module.v1.CommitService/GetCommits")
	if s != "buf.registry.module.v1.CommitService" || m != "GetCommits" {
		t.Fatalf("got %q %q", s, m)
	}
	if s, _ := SplitRPC("/healthz"); s != "" {
		t.Fatalf("expected no service, got %q", s)
	}
}

func TestProxyRecordsAndSanitises(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"echo":` + string(b) + `,"token":"abc"}`))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	var out bytes.Buffer
	p := &Proxy{Upstream: u, Out: &out, CLIVersion: "1.50.0", Scenario: "push"}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/a.b.v1.Svc/Do", strings.NewReader(`{"secret":"s","x":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), `"token":"abc"`) {
		t.Fatalf("client must receive unmodified response, got %s", body)
	}

	var e Exchange
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &e); err != nil {
		t.Fatal(err)
	}
	if e.RPC != "Do" || e.Service != "a.b.v1.Svc" || e.CLIVersion != "1.50.0" {
		t.Fatalf("bad exchange %+v", e)
	}
	raw := out.String()
	for _, leak := range []string{"supersecrettoken", `"secret":"s"`, `"token":"abc"`} {
		if strings.Contains(raw, leak) {
			t.Errorf("recording leaks %s: %s", leak, raw)
		}
	}
}

func TestMatrix(t *testing.T) {
	m := NewMatrix()
	in := `{"cli_version":"1.9.0","scenario":"push","service":"s.v1.A","rpc":"X"}
{"cli_version":"1.10.0","scenario":"build","service":"s.v1.A","rpc":"X"}
`
	if err := m.Add(strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	md := m.Markdown()
	if !strings.Contains(md, "| RPC | 1.9.0 | 1.10.0 |") || !strings.Contains(md, "`s.v1.A/X` | push | build |") {
		t.Fatalf("unexpected:\n%s", md)
	}
}

func TestRedactionEdgeCases(t *testing.T) {
	b := NewBody([]byte(`{"tokens":["actual-secret"],"n":{"api_key":{"v":"k-secret"}}}`), "application/json", false)
	if strings.Contains(string(b.JSON), "secret") {
		t.Fatalf("leak: %s", b.JSON)
	}
	bin := NewBody([]byte("\x01rawtoken"), "application/proto", false)
	if bin.Base64 != "" || bin.SHA256 == "" {
		t.Fatalf("binary body must not be kept by default: %+v", bin)
	}
}
