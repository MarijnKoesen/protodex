// SPDX-License-Identifier: Apache-2.0

// Package config loads server configuration. Precedence, lowest to highest:
// defaults, JSON config file, PROTODEX_* environment variables, flags.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Config is the server configuration.
type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string `json:"addr"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `json:"log_level"`
	// LogFormat is "text" or "json".
	LogFormat string `json:"log_format"`
	// MaxRequestBytes caps the size of a request body (and header block).
	MaxRequestBytes int64 `json:"max_request_bytes"`
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout Duration `json:"shutdown_timeout"`
}

// Duration is a time.Duration that (un)marshals as a string such as "30s".
type Duration time.Duration

// UnmarshalJSON parses a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Default returns the default configuration.
func Default() Config {
	return Config{
		Addr:            ":8080",
		LogLevel:        "info",
		LogFormat:       "text",
		MaxRequestBytes: 64 << 20,
		ShutdownTimeout: Duration(30 * time.Second),
	}
}

// Load builds a Config from a config file, environment and args.
// getenv is usually os.Getenv. Flag usage output goes to out.
func Load(args []string, getenv func(string) string, out io.Writer) (Config, error) {
	cfg := Default()

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(out)
	file := fs.String("config", getenv("PROTODEX_CONFIG"), "path to a JSON config file")
	// Flags are registered with the final defaults after file/env are applied,
	// so parse once to find -config, then layer again.
	var (
		addr      = fs.String("addr", "", "listen address")
		logLevel  = fs.String("log-level", "", "log level: debug|info|warn|error")
		logFormat = fs.String("log-format", "", "log format: text|json")
		maxBytes  = fs.Int64("max-request-bytes", 0, "maximum request body size in bytes")
		shutdown  = fs.Duration("shutdown-timeout", 0, "graceful shutdown timeout")
	)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if *file != "" {
		b, err := os.ReadFile(*file) //nolint:gosec // operator-supplied path
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", *file, err)
		}
	}

	if err := applyEnv(&cfg, getenv); err != nil {
		return Config{}, err
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["addr"] {
		cfg.Addr = *addr
	}
	if set["log-level"] {
		cfg.LogLevel = *logLevel
	}
	if set["log-format"] {
		cfg.LogFormat = *logFormat
	}
	if set["max-request-bytes"] {
		cfg.MaxRequestBytes = *maxBytes
	}
	if set["shutdown-timeout"] {
		cfg.ShutdownTimeout = Duration(*shutdown)
	}

	return cfg, cfg.Validate()
}

func applyEnv(cfg *Config, getenv func(string) string) error {
	if v := getenv("PROTODEX_ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := getenv("PROTODEX_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := getenv("PROTODEX_LOG_FORMAT"); v != "" {
		cfg.LogFormat = v
	}
	if v := getenv("PROTODEX_MAX_REQUEST_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("PROTODEX_MAX_REQUEST_BYTES: %w", err)
		}
		cfg.MaxRequestBytes = n
	}
	if v := getenv("PROTODEX_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("PROTODEX_SHUTDOWN_TIMEOUT: %w", err)
		}
		cfg.ShutdownTimeout = Duration(d)
	}
	return nil
}

// Validate checks the configuration for usable values.
func (c Config) Validate() error {
	var errs []error
	if c.Addr == "" {
		errs = append(errs, errors.New("addr must not be empty"))
	}
	if c.MaxRequestBytes <= 0 {
		errs = append(errs, errors.New("max_request_bytes must be positive"))
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("shutdown_timeout must be positive"))
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log_format %q must be text or json", c.LogFormat))
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.LogLevel)); err != nil {
		errs = append(errs, fmt.Errorf("log_level %q: %w", c.LogLevel, err))
	}
	return errors.Join(errs...)
}

// Logger builds the slog logger described by the config, writing to w.
func (c Config) Logger(w io.Writer) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(c.LogLevel)) // validated by Validate
	opts := &slog.HandlerOptions{Level: lvl}
	if c.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
