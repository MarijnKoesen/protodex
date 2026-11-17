// SPDX-License-Identifier: Apache-2.0

// Package capture implements a logging reverse proxy that records the
// Connect/HTTP traffic of the real buf CLI, and a summariser that turns the
// recordings into an RPC x CLI-version matrix. It records behaviour only.
package capture

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Body is a recorded HTTP body. JSON bodies are kept structured; anything
// else is base64 encoded.
type Body struct {
	JSON   json.RawMessage `json:"json,omitempty"`
	Base64 string          `json:"base64,omitempty"`
	Size   int             `json:"size"`
}

// Exchange is one request/response pair.
type Exchange struct {
	Time       time.Time           `json:"time"`
	CLIVersion string              `json:"cli_version"`
	Scenario   string              `json:"scenario"`
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Service    string              `json:"service,omitempty"`
	RPC        string              `json:"rpc,omitempty"`
	Status     int                 `json:"status"`
	ReqHeader  map[string][]string `json:"request_headers"`
	RespHeader map[string][]string `json:"response_headers"`
	ReqBody    Body                `json:"request_body"`
	RespBody   Body                `json:"response_body"`
}

const redacted = "REDACTED"

var (
	sensitiveHeaders = map[string]bool{
		"authorization": true, "cookie": true, "set-cookie": true,
		"proxy-authorization": true, "x-api-key": true,
	}
	sensitiveKey = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[_-]?key|authorization)`)
	// bearer-ish tokens that may appear inside free text.
	tokenText = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
)

// SplitRPC extracts service and method from a Connect path such as
// /buf.registry.module.v1.CommitService/GetCommits.
func SplitRPC(path string) (service, method string) {
	p := strings.Trim(path, "/")
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", ""
	}
	svc := p[:i]
	if j := strings.LastIndex(svc, "/"); j >= 0 {
		svc = svc[j+1:]
	}
	if !strings.Contains(svc, ".") {
		return "", ""
	}
	return svc, p[i+1:]
}

// SanitizeHeaders returns a copy of h with credentials redacted.
func SanitizeHeaders(h map[string][]string) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
			out[k] = []string{redacted}
			continue
		}
		out[k] = append([]string(nil), v...)
	}
	return out
}

// NewBody builds a sanitised Body from raw bytes and a content type.
func NewBody(raw []byte, contentType string) Body {
	b := Body{Size: len(raw)}
	if len(raw) == 0 {
		return b
	}
	if strings.Contains(contentType, "json") && json.Valid(raw) {
		var v any
		if err := json.Unmarshal(raw, &v); err == nil {
			if out, err := json.Marshal(scrub(v)); err == nil {
				b.JSON = out
				return b
			}
		}
	}
	// Binary (or non-JSON) payloads may embed credentials we cannot see
	// into; scrub bearer-looking text on a best-effort basis.
	b.Base64 = base64.StdEncoding.EncodeToString(
		[]byte(tokenText.ReplaceAllString(string(raw), "${1}"+redacted)))
	return b
}

func scrub(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if _, isStr := val.(string); isStr && sensitiveKey.MatchString(k) {
				t[k] = redacted
				continue
			}
			t[k] = scrub(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = scrub(t[i])
		}
		return t
	case string:
		return tokenText.ReplaceAllString(t, "${1}"+redacted)
	}
	return v
}
