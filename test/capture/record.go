// SPDX-License-Identifier: Apache-2.0

// Package capture implements a logging reverse proxy that records the
// Connect/HTTP traffic of the real buf CLI, and a summariser that turns the
// recordings into an RPC x CLI-version matrix. It records behaviour only.
package capture

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	// SHA256 identifies payloads whose content was not kept (binary bodies
	// are dropped by default because credentials cannot be scrubbed from
	// them reliably).
	SHA256 string `json:"sha256,omitempty"`
	Size   int    `json:"size"`
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
// Non-JSON bodies are only kept (base64) when keepBinary is true.
func NewBody(raw []byte, contentType string, keepBinary bool) Body {
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
	sum := sha256.Sum256(raw)
	b.SHA256 = hex.EncodeToString(sum[:])
	if keepBinary {
		b.Base64 = base64.StdEncoding.EncodeToString(
			[]byte(tokenText.ReplaceAllString(string(raw), "${1}"+redacted)))
	}
	return b
}

func scrub(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if sensitiveKey.MatchString(k) && val != nil {
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
