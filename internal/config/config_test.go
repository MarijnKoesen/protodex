// SPDX-License-Identifier: Apache-2.0

package config

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	cfg, err := Load(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("got %+v", cfg)
	}
}

func TestPrecedence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"addr":":1","log_level":"warn","shutdown_timeout":"5s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"-config", p, "-addr", ":3"},
		env(map[string]string{"PROTODEX_ADDR": ":2", "PROTODEX_LOG_LEVEL": "debug"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":3" || cfg.LogLevel != "debug" || time.Duration(cfg.ShutdownTimeout) != 5*time.Second {
		t.Fatalf("got %+v", cfg)
	}
}

func TestInvalid(t *testing.T) {
	for name, args := range map[string][]string{
		"bad level":  {"-log-level", "loud"},
		"bad format": {"-log-format", "xml"},
		"bad max":    {"-max-request-bytes", "-1"},
		"bad flag":   {"-nope"},
	} {
		if _, err := Load(args, env(nil), io.Discard); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := Load(nil, env(map[string]string{"PROTODEX_MAX_REQUEST_BYTES": "x"}), io.Discard); err == nil {
		t.Error("expected env parse error")
	}
	if _, err := Load([]string{"-config", "/nonexistent"}, env(nil), io.Discard); err == nil {
		t.Error("expected missing file error")
	}
}

func TestTracingConfig(t *testing.T) {
	cfg, err := Load(nil, env(nil), io.Discard)
	if err != nil || cfg.TracingEnabled {
		t.Fatalf("tracing must default off: %+v %v", cfg, err)
	}
	cfg, err = Load([]string{"-otlp-endpoint", "http://c:4318"},
		env(map[string]string{"PROTODEX_TRACING_ENABLED": "true"}), io.Discard)
	if err != nil || !cfg.TracingEnabled || cfg.OTLPEndpoint != "http://c:4318" {
		t.Fatalf("got %+v %v", cfg, err)
	}
	if _, err := Load(nil, env(map[string]string{"PROTODEX_TRACING_ENABLED": "x"}), io.Discard); err == nil {
		t.Error("expected parse error")
	}
}

func TestOIDCConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	body := `{"oidc":{"issuer":"https://idp.example.com","client_id":"pd","redirect_url":"https://pd.example.com/auth/callback",
	  "allowed_domains":["example.com"],"group_roles":{"admins":"admin"}}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"-config", p}, env(map[string]string{"PROTODEX_OIDC_CLIENT_SECRET": "s"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OIDC.Enabled() || cfg.OIDC.ClientSecret != "s" || cfg.OIDC.GroupsClaim != "groups" || cfg.OIDC.GroupRoles["admins"] != "admin" {
		t.Fatalf("got %+v", cfg.OIDC)
	}
}

func TestOIDCInvalid(t *testing.T) {
	ok := Default().OIDC
	ok.Issuer, ok.ClientID, ok.RedirectURL = "https://idp", "c", "https://pd/auth/callback"
	ok.DefaultRole = "viewer"
	for name, mod := range map[string]func(*OIDC){
		"bad issuer":   func(o *OIDC) { o.Issuer = "idp" },
		"no client id": func(o *OIDC) { o.ClientID = "" },
		"bad redirect": func(o *OIDC) { o.RedirectURL = "/auth/callback" },
		"bad role":     func(o *OIDC) { o.GroupRoles = map[string]string{"g": "root"} },
		"bad default":  func(o *OIDC) { o.DefaultRole = "root" },
		"nobody":       func(o *OIDC) { o.DefaultRole = "" },
	} {
		o := ok
		mod(&o)
		c := Default()
		c.OIDC = o
		if c.Validate() == nil {
			t.Errorf("%s: want error", name)
		}
	}
	c := Default()
	c.OIDC = ok
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGCConfig(t *testing.T) {
	cfg, err := Load(nil, env(nil), io.Discard)
	if err != nil || time.Duration(cfg.GCInterval) != time.Hour || time.Duration(cfg.GCGracePeriod) != 24*time.Hour || cfg.GCDryRun {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"gc_interval":"0s","gc_grace_period":"2h","gc_dry_run":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load([]string{"-config", p}, env(map[string]string{"PROTODEX_GC_GRACE_PERIOD": "3h"}), io.Discard)
	if err != nil || cfg.GCInterval != 0 || time.Duration(cfg.GCGracePeriod) != 3*time.Hour || !cfg.GCDryRun {
		t.Fatalf("file+env: %+v %v", cfg, err)
	}
	cfg, err = Load(nil, env(map[string]string{"PROTODEX_GC_INTERVAL": "10m", "PROTODEX_GC_DRY_RUN": "false"}), io.Discard)
	if err != nil || time.Duration(cfg.GCInterval) != 10*time.Minute || cfg.GCDryRun {
		t.Fatalf("env: %+v %v", cfg, err)
	}
	for _, e := range []map[string]string{
		{"PROTODEX_GC_INTERVAL": "soon"},
		{"PROTODEX_GC_INTERVAL": "-1m"},
		{"PROTODEX_GC_GRACE_PERIOD": "0s"},
		{"PROTODEX_GC_DRY_RUN": "maybe"},
	} {
		if _, err := Load(nil, env(e), io.Discard); err == nil {
			t.Errorf("%v: expected error", e)
		}
	}
}

func TestCITrustConfig(t *testing.T) {
	cfg, err := Load(nil, env(nil), io.Discard)
	if err != nil || cfg.CITrust.Enabled || cfg.CITrust.GitHubIssuer != GitHubActionsIssuer || time.Duration(cfg.CITrust.TokenTTL) != 15*time.Minute {
		t.Fatalf("defaults: %+v %v", cfg.CITrust, err)
	}
	p := filepath.Join(t.TempDir(), "c.json")
	body := `{"ci_trust":{"enabled":true,"kubernetes":[{"issuer":"https://k8s.example.com","jwks_url":"https://k8s.example.com/openid/v1/jwks"}]}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load([]string{"-config", p}, env(map[string]string{"PROTODEX_CI_TRUST_TOKEN_TTL": "30m"}), io.Discard)
	if err != nil || !cfg.CITrust.Enabled || len(cfg.CITrust.Kubernetes) != 1 || time.Duration(cfg.CITrust.TokenTTL) != 30*time.Minute {
		t.Fatalf("file+env: %+v %v", cfg.CITrust, err)
	}
	cfg, err = Load(nil, env(map[string]string{"PROTODEX_CI_TRUST_ENABLED": "1", "PROTODEX_CI_TRUST_GITHUB_ISSUER": "https://ghes.example.com/_services/token",
		"PROTODEX_CI_TRUST_KUBERNETES_ISSUERS": "https://a.example.com, https://b.example.com"}), io.Discard)
	if err != nil || !cfg.CITrust.Enabled || cfg.CITrust.GitHubIssuer != "https://ghes.example.com/_services/token" ||
		len(cfg.CITrust.Kubernetes) != 2 || cfg.CITrust.Kubernetes[1].Issuer != "https://b.example.com" {
		t.Fatalf("env: %+v %v", cfg.CITrust, err)
	}
	for _, bad := range []map[string]string{
		{"PROTODEX_CI_TRUST_ENABLED": "maybe"},
		{"PROTODEX_CI_TRUST_TOKEN_TTL": "soon"},
	} {
		if _, err := Load(nil, env(bad), io.Discard); err == nil {
			t.Errorf("%v: want error", bad)
		}
	}
}

func TestCITrustInvalid(t *testing.T) {
	for name, mod := range map[string]func(*CITrust){
		"bad github issuer": func(c *CITrust) { c.GitHubIssuer = "github" },
		"bad k8s issuer":    func(c *CITrust) { c.Kubernetes = []KubernetesIssuer{{Issuer: "k8s"}} },
		"bad jwks url":      func(c *CITrust) { c.Kubernetes = []KubernetesIssuer{{Issuer: "https://k", JWKSURL: "/keys"}} },
		"duplicate issuer":  func(c *CITrust) { c.Kubernetes = []KubernetesIssuer{{Issuer: GitHubActionsIssuer}} },
		"zero ttl":          func(c *CITrust) { c.TokenTTL = 0 },
		"ttl too long":      func(c *CITrust) { c.TokenTTL = Duration(2 * time.Hour) },
	} {
		c := Default()
		c.CITrust.Enabled = true
		mod(&c.CITrust)
		if c.Validate() == nil {
			t.Errorf("%s: want error", name)
		}
	}
	c := Default()
	c.CITrust.Enabled = true
	c.CITrust.Kubernetes = []KubernetesIssuer{{Issuer: "https://k8s.example.com"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
