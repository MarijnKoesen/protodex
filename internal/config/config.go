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
	"net/url"
	"os"
	"strconv"
	"strings"
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
	// DBPath is the SQLite database file. When set, serve opens it (taking
	// the exclusive process lock) and refuses to start if it is in use.
	DBPath string `json:"db_path"`
	// BlobDir is the directory file contents are stored in (content-addressed).
	// Default: the database path plus ".blobs". Only used with db_path.
	BlobDir string `json:"blob_dir"`
	// MaxPushFiles and MaxPushFileBytes bound one pushed module; zero selects
	// the defaults (5000 files, 10 MiB per file).
	MaxPushFiles     int   `json:"max_push_files"`
	MaxPushFileBytes int64 `json:"max_push_file_bytes"`
	// GCInterval is how often unreferenced blobs are garbage-collected
	// (only with db_path). "0s" disables the collector. Default 1h.
	GCInterval Duration `json:"gc_interval"`
	// GCGracePeriod is the minimum age of an unreferenced blob before it is
	// deleted. Default 24h.
	GCGracePeriod Duration `json:"gc_grace_period"`
	// GCDryRun logs and counts what garbage collection would delete without
	// deleting anything.
	GCDryRun bool `json:"gc_dry_run"`
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout Duration `json:"shutdown_timeout"`
	// TracingEnabled turns on OpenTelemetry tracing with an OTLP/HTTP exporter.
	TracingEnabled bool `json:"tracing_enabled"`
	// OTLPEndpoint is the OTLP/HTTP collector URL; empty uses the standard
	// OTEL_EXPORTER_OTLP_* environment variables.
	OTLPEndpoint string `json:"otlp_endpoint"`
	// OIDC configures user login through an OpenID Connect provider. It is
	// disabled while OIDC.Issuer is empty.
	OIDC OIDC `json:"oidc"`
}

// OIDC configures the OpenID Connect authorization-code login (Dex, Keycloak,
// Entra ID, Google, ...). The client secret is best supplied through
// PROTODEX_OIDC_CLIENT_SECRET rather than the config file.
type OIDC struct {
	// Issuer is the provider's issuer URL (discovery is at
	// Issuer/.well-known/openid-configuration).
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	// RedirectURL is the externally visible URL of /auth/callback, as
	// registered with the provider.
	RedirectURL string `json:"redirect_url"`
	// Scopes are requested in addition to "openid". Default: profile, email
	// and (for group mapping) groups.
	Scopes []string `json:"scopes"`
	// AllowedDomains restricts login to verified e-mail addresses in these
	// domains. Empty allows any user the provider authenticates.
	AllowedDomains []string `json:"allowed_domains"`
	// UsernameClaim names the claim a new user's name is derived from.
	// Default: preferred_username, falling back to the e-mail local part.
	UsernameClaim string `json:"username_claim"`
	// GroupsClaim names the claim holding the user's groups. Default "groups".
	GroupsClaim string `json:"groups_claim"`
	// GroupRoles maps a group to a role (viewer, member or admin). A user
	// gets the highest role of their groups.
	GroupRoles map[string]string `json:"group_roles"`
	// DefaultRole applies when no group matches; empty rejects such users.
	DefaultRole string `json:"default_role"`
	// TokenTTL is the lifetime of personal access tokens minted at login.
	// Default 720h (30 days).
	TokenTTL Duration `json:"token_ttl"`
}

// Enabled reports whether OIDC login is configured.
func (o OIDC) Enabled() bool { return o.Issuer != "" }

// Roles in ascending order of privilege.
const (
	RoleViewer = "viewer"
	RoleMember = "member"
	RoleAdmin  = "admin"
)

// RoleRank orders roles; unknown roles rank 0.
func RoleRank(r string) int {
	switch r {
	case RoleViewer:
		return 1
	case RoleMember:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

func (o OIDC) validate() []error {
	if !o.Enabled() {
		return nil
	}
	var errs []error
	if u, err := url.Parse(o.Issuer); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		errs = append(errs, fmt.Errorf("oidc.issuer %q must be an http(s) URL", o.Issuer))
	}
	if o.ClientID == "" {
		errs = append(errs, errors.New("oidc.client_id is required"))
	}
	if u, err := url.Parse(o.RedirectURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		errs = append(errs, fmt.Errorf("oidc.redirect_url %q must be an absolute http(s) URL", o.RedirectURL))
	}
	for g, r := range o.GroupRoles {
		if RoleRank(r) == 0 {
			errs = append(errs, fmt.Errorf("oidc.group_roles[%q]: unknown role %q (want viewer, member or admin)", g, r))
		}
	}
	if o.DefaultRole != "" && RoleRank(o.DefaultRole) == 0 {
		errs = append(errs, fmt.Errorf("oidc.default_role: unknown role %q", o.DefaultRole))
	}
	if o.DefaultRole == "" && len(o.GroupRoles) == 0 {
		errs = append(errs, errors.New("oidc: set group_roles and/or default_role, otherwise nobody can log in"))
	}
	if o.TokenTTL < 0 {
		errs = append(errs, errors.New("oidc.token_ttl must not be negative"))
	}
	return errs
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
		GCInterval:      Duration(time.Hour),
		GCGracePeriod:   Duration(24 * time.Hour),
		OIDC: OIDC{
			Scopes:      []string{"profile", "email", "groups"},
			GroupsClaim: "groups",
			TokenTTL:    Duration(720 * time.Hour),
		},
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
		dbPath    = fs.String("db-path", "", "SQLite database path")
		shutdown  = fs.Duration("shutdown-timeout", 0, "graceful shutdown timeout")
		tracing   = fs.Bool("tracing", false, "enable OpenTelemetry tracing (OTLP/HTTP)")
		otlp      = fs.String("otlp-endpoint", "", "OTLP/HTTP collector URL")
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
	if set["db-path"] {
		cfg.DBPath = *dbPath
	}
	if set["shutdown-timeout"] {
		cfg.ShutdownTimeout = Duration(*shutdown)
	}
	if set["tracing"] {
		cfg.TracingEnabled = *tracing
	}
	if set["otlp-endpoint"] {
		cfg.OTLPEndpoint = *otlp
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
	if v := getenv("PROTODEX_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := getenv("PROTODEX_BLOB_DIR"); v != "" {
		cfg.BlobDir = v
	}
	if v := getenv("PROTODEX_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("PROTODEX_SHUTDOWN_TIMEOUT: %w", err)
		}
		cfg.ShutdownTimeout = Duration(d)
	}
	for _, e := range []struct {
		name string
		dst  *Duration
	}{{"PROTODEX_GC_INTERVAL", &cfg.GCInterval}, {"PROTODEX_GC_GRACE_PERIOD", &cfg.GCGracePeriod}} {
		if v := getenv(e.name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s: %w", e.name, err)
			}
			*e.dst = Duration(d)
		}
	}
	if v := getenv("PROTODEX_GC_DRY_RUN"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("PROTODEX_GC_DRY_RUN: %w", err)
		}
		cfg.GCDryRun = b
	}
	if v := getenv("PROTODEX_TRACING_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("PROTODEX_TRACING_ENABLED: %w", err)
		}
		cfg.TracingEnabled = b
	}
	if v := getenv("PROTODEX_OTLP_ENDPOINT"); v != "" {
		cfg.OTLPEndpoint = v
	}
	if v := getenv("PROTODEX_OIDC_ISSUER"); v != "" {
		cfg.OIDC.Issuer = v
	}
	if v := getenv("PROTODEX_OIDC_CLIENT_ID"); v != "" {
		cfg.OIDC.ClientID = v
	}
	if v := getenv("PROTODEX_OIDC_CLIENT_SECRET"); v != "" {
		cfg.OIDC.ClientSecret = v
	}
	if v := getenv("PROTODEX_OIDC_REDIRECT_URL"); v != "" {
		cfg.OIDC.RedirectURL = v
	}
	if v := getenv("PROTODEX_OIDC_ALLOWED_DOMAINS"); v != "" {
		cfg.OIDC.AllowedDomains = strings.Split(v, ",")
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
	if c.MaxPushFiles < 0 || c.MaxPushFileBytes < 0 {
		errs = append(errs, errors.New("max_push_files and max_push_file_bytes must not be negative"))
	}
	if c.GCInterval < 0 {
		errs = append(errs, errors.New("gc_interval must not be negative (0s disables garbage collection)"))
	}
	if c.GCGracePeriod <= 0 {
		errs = append(errs, errors.New("gc_grace_period must be positive"))
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
	errs = append(errs, c.OIDC.validate()...)
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
