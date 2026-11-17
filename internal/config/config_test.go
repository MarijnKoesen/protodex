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
