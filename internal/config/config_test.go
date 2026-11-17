// SPDX-License-Identifier: Apache-2.0

package config

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	cfg, err := Load(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != Default() {
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
