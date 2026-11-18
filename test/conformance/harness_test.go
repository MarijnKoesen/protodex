// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/blob/fsblob"
	"github.com/marijnkoesen/protodex/internal/module"
	"github.com/marijnkoesen/protodex/internal/observability"
	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/server"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/test/conformance/bufdl"
)

// T is the part of *testing.T scenarios use. The fault self-test substitutes
// a recorder that captures failures instead of failing the test.
type T interface {
	Helper()
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
	TempDir() string
	Cleanup(func())
	Failed() bool
}

// bufCLI is one buf binary under test.
type bufCLI struct {
	Path    string
	Version string

	mu   sync.Mutex
	help map[string]string
}

func newBufCLI(path string) (*bufCLI, error) {
	// Scenarios run in a temporary working directory, so keep the path absolute.
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", path, err)
	}
	path = abs
	out, err := exec.Command(path, "--version").Output() //nolint:gosec // the configured buf binary
	if err != nil {
		return nil, fmt.Errorf("%s --version: %w", path, err)
	}
	return &bufCLI{Path: path, Version: strings.TrimSpace(string(out)), help: map[string]string{}}, nil
}

// atLeast reports whether the CLI is version v or newer.
func (c *bufCLI) atLeast(v string) bool { return bufdl.CompareVersions(c.Version, v) >= 0 }

// helpText returns `buf <path> --help`, or "" if that command does not
// exist (cobra then prints the parent's usage).
func (c *bufCLI) helpText(path ...string) string {
	key := strings.Join(path, " ")
	c.mu.Lock()
	defer c.mu.Unlock()
	if h, ok := c.help[key]; ok {
		return h
	}
	cmd := exec.Command(c.Path, append(path, "--help")...) //nolint:gosec // the configured buf binary
	cmd.Env = []string{"HOME=" + os.TempDir(), "PATH=" + os.Getenv("PATH")}
	out, _ := cmd.CombinedOutput()
	h := string(out)
	if !strings.Contains(h, "\n  buf "+key+" ") && !strings.Contains(h, "\n  buf "+key+"\n") {
		h = ""
	}
	c.help[key] = h
	return h
}

// has reports whether the CLI has the command path.
func (c *bufCLI) has(path ...string) bool { return c.helpText(path...) != "" }

// first returns the first command path the CLI has, or nil.
func (c *bufCLI) first(paths ...[]string) []string {
	for _, p := range paths {
		if c.has(p...) {
			return p
		}
	}
	return nil
}

var (
	clisOnce sync.Once
	clis     []*bufCLI
	clisErr  error
	clisSkip string
)

// bufCLIs resolves the buf binaries under test (see the package doc).
func bufCLIs(t *testing.T) []*bufCLI {
	t.Helper()
	clisOnce.Do(func() {
		var paths []string
		switch {
		case os.Getenv("PROTODEX_CONFORMANCE_BUF_VERSIONS") != "":
			paths, clisErr = downloadVersions(os.Getenv("PROTODEX_CONFORMANCE_BUF_VERSIONS"))
		case os.Getenv("PROTODEX_CONFORMANCE_BUF") != "":
			paths = strings.Split(os.Getenv("PROTODEX_CONFORMANCE_BUF"), ",")
		case os.Getenv("PROTODEX_TEST_BUF") != "":
			paths = []string{os.Getenv("PROTODEX_TEST_BUF")}
		default:
			p, err := exec.LookPath("buf")
			if err != nil {
				clisSkip = "buf CLI not found: set PROTODEX_CONFORMANCE_BUF_VERSIONS (e.g. pr), PROTODEX_CONFORMANCE_BUF or PROTODEX_TEST_BUF, or put buf on PATH"
				return
			}
			paths = []string{p}
		}
		for _, p := range paths {
			if clisErr != nil {
				return
			}
			var c *bufCLI
			c, clisErr = newBufCLI(strings.TrimSpace(p))
			clis = append(clis, c)
		}
	})
	if clisSkip != "" {
		t.Skip(clisSkip)
	}
	if clisErr != nil {
		t.Fatal(clisErr)
	}
	return clis
}

func downloadVersions(spec string) ([]string, error) {
	var versions []string
	var err error
	switch spec {
	case "all", "pr":
		versions, err = bufdl.Supported(spec == "pr")
	default:
		versions = strings.Split(spec, ",")
	}
	if err != nil {
		return nil, err
	}
	dir := os.Getenv("PROTODEX_CONFORMANCE_CACHE")
	if dir == "" {
		dir = bufdl.DefaultCacheDir()
	}
	d := &bufdl.Downloader{Dir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var paths []string
	for _, v := range versions {
		p, err := d.Get(ctx, strings.TrimSpace(v))
		if err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Env is a running protodex server and a buf CLI configured to reach it.
type Env struct {
	t     T
	cli   *bufCLI
	Host  string // host:port, the module remote
	St    store.Store
	Token string
	// NoToken leaves BUF_TOKEN unset so the CLI uses .netrc credentials.
	NoToken bool

	home, cache, certFile string
	log                   *syncBuffer
	logger                *slog.Logger
}

// newEnv starts protodex on backend b (with fault injected, if any), creates
// user "admin" with a token and organization "acme", and prepares an
// isolated HOME and cache for the CLI. If the test fails, the server's
// request log is printed.
func newEnv(t T, cli *bufCLI, b Backend, fault string) *Env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("protodex request log (backend %s, buf %s, fault %q):\n%s", b.Name, cli.Version, fault, logBuf.String())
		}
	})

	st, err := b.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open %s store: %v", b.Name, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	secret, _, err := auth.Bootstrap(ctx, st, "admin", "")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := owner.Create(ctx, st, owner.Organization, "acme"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	blobs, err := fsblob.New(filepath.Join(dir, "blobs"), 0)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	h := server.Handler(logger, 64<<20, observability.New(), server.WithStore(st), server.WithBlobs(blobs))
	// The CLI only speaks TLS: serve HTTPS with a test certificate it trusts
	// through SSL_CERT_FILE.
	ts := httptest.NewUnstartedServer(errorBodies(logger, withFault(logger, h, fault)))
	ts.EnableHTTP2 = true
	ts.Config.ErrorLog = slog.NewLogLogger(logger.Handler(), slog.LevelWarn)
	ts.StartTLS()
	t.Cleanup(ts.Close)

	e := &Env{
		t: t, cli: cli, Host: strings.TrimPrefix(ts.URL, "https://"), St: st, Token: secret,
		home: filepath.Join(dir, "home"), cache: filepath.Join(dir, "cache"),
		certFile: filepath.Join(dir, "cert.pem"), log: logBuf, logger: logger,
	}
	if err := os.WriteFile(e.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.Mkdir(e.home, 0o700); err != nil {
		t.Fatalf("home: %v", err)
	}
	return e
}

// errorBodies logs the body of every non-200 response (Connect errors carry
// their code and message there), capped at 1 KiB.
func errorBodies(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &capturingWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		if cw.status != 0 && cw.status != http.StatusOK {
			log.Warn("error response", "path", r.URL.Path, "status", cw.status, "body", cw.body.String())
		}
		if m := w.Header().Get("Grpc-Message"); m != "" {
			log.Warn("grpc error", "path", r.URL.Path, "grpc-status", w.Header().Get("Grpc-Status"), "message", m)
		}
	})
}

type capturingWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *capturingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *capturingWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.status != http.StatusOK && c.body.Len() < 1024 {
		c.body.Write(p[:min(len(p), 1024-c.body.Len())])
	}
	return c.ResponseWriter.Write(p)
}

func (c *capturingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *capturingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// result is the outcome of one CLI run.
type result struct {
	Stdout, Stderr string
	Err            error
}

// Exec runs buf in dir (a fresh temp dir if empty) with stdin and returns
// its result without judging it.
func (e *Env) Exec(dir, stdin string, args ...string) result {
	e.t.Helper()
	if dir == "" {
		dir = e.t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.cli.Path, args...) //nolint:gosec // the configured buf binary
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + e.home,
		"XDG_CONFIG_HOME=" + filepath.Join(e.home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(e.home, ".cache"),
		"BUF_CACHE_DIR=" + e.cache,
		"SSL_CERT_FILE=" + e.certFile,
	}
	if !e.NoToken {
		cmd.Env = append(cmd.Env, "BUF_TOKEN="+e.Token)
	}
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	e.logger.Info("$ buf " + strings.Join(args, " "))
	err := cmd.Run()
	r := result{Stdout: stdout.String(), Stderr: stderr.String(), Err: err}
	e.logger.Info("buf exited", "err", err, "stderr", strings.TrimSpace(r.Stderr))
	return r
}

// OK runs buf in dir and fails the test unless it succeeds; it returns stdout.
func (e *Env) OK(dir string, args ...string) string {
	e.t.Helper()
	r := e.Exec(dir, "", args...)
	if r.Err != nil {
		e.t.Fatalf("buf %s (in %s): %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), dir, r.Err, r.Stdout, r.Stderr)
	}
	return r.Stdout
}

// Fails runs buf in dir and fails the test unless it fails; it returns
// stderr.
func (e *Env) Fails(dir string, args ...string) string {
	e.t.Helper()
	r := e.Exec(dir, "", args...)
	if r.Err == nil {
		e.t.Fatalf("buf %s (in %s) succeeded, want failure\nstdout: %s\nstderr: %s", strings.Join(args, " "), dir, r.Stdout, r.Stderr)
	}
	return r.Stderr
}

// Mod returns the full remote name of acme/<name>.
func (e *Env) Mod(name string) string { return e.Host + "/acme/" + name }

// CreateModule creates acme/<name> with the CLI, or through the store when
// the CLI has no `registry module create` (buf < 1.40).
func (e *Env) CreateModule(name string) {
	e.t.Helper()
	if e.cli.has("registry", "module", "create") {
		e.OK("", "registry", "module", "create", e.Mod(name), "--visibility=public")
		return
	}
	ctx := context.Background()
	o, err := e.St.GetOwner(ctx, "acme")
	if err != nil {
		e.t.Fatalf("get owner: %v", err)
	}
	if _, err := e.St.CreateModule(ctx, store.Module{OwnerID: o.ID, Name: name, Visibility: store.VisibilityPublic, DefaultLabel: module.DefaultLabel}); err != nil {
		e.t.Fatalf("create module: %v", err)
	}
	e.t.Logf("buf %s has no `registry module create`; created acme/%s through the store", e.cli.Version, name)
}

// Module reads acme/<name> from the store.
func (e *Env) Module(name string) store.Module {
	e.t.Helper()
	ctx := context.Background()
	o, err := e.St.GetOwner(ctx, "acme")
	if err != nil {
		e.t.Fatalf("get owner: %v", err)
	}
	m, err := e.St.GetModule(ctx, o.ID, name)
	if err != nil {
		e.t.Fatalf("get module %s: %v", name, err)
	}
	return m
}

// Workspace writes a v2 workspace holding one module acme/<name> with deps
// (full remote names) and files, and returns its directory.
func (e *Env) Workspace(name string, deps []string, files map[string]string) string {
	e.t.Helper()
	dir := e.t.TempDir()
	y := "version: v2\nmodules:\n  - path: .\n    name: " + e.Mod(name) + "\n"
	if len(deps) > 0 {
		y += "deps:\n"
		for _, d := range deps {
			y += "  - " + d + "\n"
		}
	}
	// The STANDARD lint category replaced DEFAULT (still accepted, with a
	// warning) between buf 1.35.1 and 1.40.1.
	lint := "STANDARD"
	if !e.cli.atLeast("1.40.0") {
		lint = "DEFAULT"
	}
	y += "lint:\n  use:\n    - " + lint + "\nbreaking:\n  use:\n    - FILE\n"
	all := map[string]string{"buf.yaml": y}
	for p, c := range files {
		all[p] = c
	}
	e.WriteFiles(dir, all)
	return dir
}

// WriteFiles writes files (relative paths) under dir.
func (e *Env) WriteFiles(dir string, files map[string]string) {
	e.t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil { //nolint:gosec // test temp dir
			e.t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil { //nolint:gosec // test temp dir
			e.t.Fatalf("write %s: %v", p, err)
		}
	}
}

// ClearCache empties the CLI's module cache, so the next command downloads
// (and verifies) dependencies again.
func (e *Env) ClearCache() {
	e.t.Helper()
	if err := os.RemoveAll(e.cache); err != nil { //nolint:gosec // the env's temp cache dir
		e.t.Fatalf("clear cache: %v", err)
	}
}

// ReadFile reads dir/path, failing the test if it is missing.
func (e *Env) ReadFile(dir, path string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, path)) //nolint:gosec // test temp dir
	if err != nil {
		e.t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// Push runs `buf push` in dir and returns the commit id it prints.
func (e *Env) Push(dir string, args ...string) string {
	e.t.Helper()
	out := strings.TrimSpace(e.OK(dir, append([]string{"push"}, args...)...))
	lines := strings.Split(out, "\n")
	last := lines[len(lines)-1]
	id := last[strings.LastIndex(last, ":")+1:]
	if len(id) != 32 {
		e.t.Fatalf("buf push printed %q, want <module>:<32-char commit id>", out)
	}
	return id
}

// LabelCommit returns the stored commit that label of acme/<name> points to.
func (e *Env) LabelCommit(name, label string) store.Commit {
	e.t.Helper()
	ctx := context.Background()
	m := e.Module(name)
	l, err := e.St.GetLabel(ctx, m.ID, label)
	if err != nil {
		e.t.Fatalf("get label %s:%s: %v", name, label, err)
	}
	c, err := e.St.GetCommit(ctx, m.ID, l.CommitID)
	if err != nil {
		e.t.Fatalf("get commit: %v", err)
	}
	return c
}

// mustContain fails unless s contains every want.
func mustContain(t T, what, s string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(s, w) {
			t.Fatalf("%s: missing %q in:\n%s", what, w, s)
		}
	}
}

// mustNotContain fails if s contains any of bad.
func mustNotContain(t T, what, s string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(s, b) {
			t.Fatalf("%s: unexpected %q in:\n%s", what, b, s)
		}
	}
}

var (
	pluginOnce sync.Once
	pluginPath string
	pluginErr  error
)

// plugin returns the path of the protoc-gen-conformance binary, building it
// on first use.
func plugin(t T) string {
	t.Helper()
	pluginOnce.Do(func() {
		dir, err := os.MkdirTemp("", "protodex-conformance-plugin-")
		if err != nil {
			pluginErr = err
			return
		}
		pluginPath = filepath.Join(dir, "protoc-gen-conformance")
		goBin := filepath.Join(os.Getenv("GOROOT"), "bin", "go")
		if _, err := os.Stat(goBin); err != nil { //nolint:gosec // GOROOT of the test run
			goBin = "go"
		}
		cmd := exec.Command(goBin, "build", "-o", pluginPath, "./cmd/protoc-gen-conformance") //nolint:gosec // builds this package's plugin
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			pluginErr = fmt.Errorf("build plugin: %w\n%s", err, out.String())
		}
	})
	if pluginErr != nil {
		t.Fatalf("%v", pluginErr)
	}
	return pluginPath
}

// report collects scenario outcomes for PROTODEX_CONFORMANCE_REPORT.
var report = struct {
	sync.Mutex
	rows map[[3]string]string // backend, version, scenario → pass/FAIL/skip
}{rows: map[[3]string]string{}}

func record(backend, version, scenario, outcome string) {
	report.Lock()
	defer report.Unlock()
	report.rows[[3]string{backend, version, scenario}] = outcome
}

// writeReport renders the collected outcomes as a Markdown table, one row
// per backend and buf version.
func writeReport(w io.Writer) error {
	report.Lock()
	defer report.Unlock()
	type key struct{ backend, version string }
	var keys []key
	seen := map[key]bool{}
	for k := range report.rows {
		kk := key{k[0], k[1]}
		if !seen[kk] {
			seen[kk] = true
			keys = append(keys, kk)
		}
	}
	sortKeys := func(i, j int) bool {
		if keys[i].backend != keys[j].backend {
			return keys[i].backend < keys[j].backend
		}
		return bufdl.CompareVersions(keys[i].version, keys[j].version) < 0
	}
	sort.Slice(keys, sortKeys)
	var b strings.Builder
	b.WriteString("| backend | buf |")
	for _, s := range scenarios {
		b.WriteString(" " + s.Name + " |")
	}
	b.WriteString("\n|---|---|")
	for range scenarios {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "| %s | %s |", k.backend, k.version)
		for _, s := range scenarios {
			o := report.rows[[3]string{k.backend, k.version, s.Name}]
			if o == "" {
				o = "–"
			}
			b.WriteString(" " + o + " |")
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
