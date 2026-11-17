// SPDX-License-Identifier: Apache-2.0

// Package clitest drives the real buf CLI against an in-process protodex
// server. Tests skip when no buf binary is available: set PROTODEX_TEST_BUF
// to its path, or put buf on PATH.
package clitest

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/blob/fsblob"
	"github.com/marijnkoesen/protodex/internal/observability"
	"github.com/marijnkoesen/protodex/internal/owner"
	"github.com/marijnkoesen/protodex/internal/server"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

// env is a running protodex server plus a buf CLI configured to reach it.
type env struct {
	t      *testing.T
	buf    string
	host   string // host:port used as the module remote
	st     store.Store
	cliEnv []string
}

func bufBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("PROTODEX_TEST_BUF"); p != "" {
		return p
	}
	p, err := exec.LookPath("buf")
	if err != nil {
		t.Skip("buf CLI not found; set PROTODEX_TEST_BUF or put buf on PATH")
	}
	return p
}

func newEnv(t *testing.T) *env {
	t.Helper()
	bin := bufBinary(t)
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(filepath.Join(dir, "protodex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	secret, _, err := auth.Bootstrap(ctx, st, "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Create(ctx, st, owner.Organization, "acme"); err != nil {
		t.Fatal(err)
	}

	// The CLI only speaks TLS: serve HTTPS with a test certificate it trusts
	// through SSL_CERT_FILE.
	blobs, err := fsblob.New(filepath.Join(dir, "blobs"), 0)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(server.Handler(slog.New(slog.DiscardHandler), 1<<20, observability.New(),
		server.WithStore(st), server.WithBlobs(blobs)))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	certFile := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return &env{
		t:    t,
		buf:  bin,
		host: strings.TrimPrefix(ts.URL, "https://"),
		st:   st,
		cliEnv: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"BUF_CACHE_DIR=" + filepath.Join(dir, "cache"),
			"BUF_TOKEN=" + secret,
			"SSL_CERT_FILE=" + certFile,
		},
	}
}

// run runs buf with args and returns stdout; wantErr selects whether a
// non-zero exit is expected.
func (e *env) run(wantErr bool, args ...string) (string, string) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.buf, args...) //nolint:gosec // test runs the configured buf binary
	cmd.Env = e.cliEnv
	cmd.Dir = e.t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if (err != nil) != wantErr {
		e.t.Fatalf("buf %s: err = %v, wantErr %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, wantErr, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

func (e *env) ok(args ...string) string {
	e.t.Helper()
	out, _ := e.run(false, args...)
	return out
}

func (e *env) fails(args ...string) string {
	e.t.Helper()
	_, errOut := e.run(true, args...)
	return errOut
}

func (e *env) json(v any, args ...string) {
	e.t.Helper()
	out := e.ok(append(args, "--format=json")...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		e.t.Fatalf("buf %s: decoding %q: %v", strings.Join(args, " "), out, err)
	}
}

// module reads a module straight from the store.
func (e *env) module(ownerName, name string) store.Module {
	e.t.Helper()
	ctx := context.Background()
	o, err := e.st.GetOwner(ctx, ownerName)
	if err != nil {
		e.t.Fatal(err)
	}
	m, err := e.st.GetModule(ctx, o.ID, name)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func TestModuleCommands(t *testing.T) {
	e := newEnv(t)
	mod := e.host + "/acme/weather"

	e.ok("registry", "module", "create", mod, "--visibility=public")
	if m := e.module("acme", "weather"); m.Visibility != store.VisibilityPublic {
		t.Fatalf("stored module = %+v", m)
	}
	var info map[string]any
	e.json(&info, "registry", "module", "info", mod)
	t.Logf("module info: %v", info)
	if info["name"] != "weather" || info["owner"] != "acme" || info["default_label_name"] != "main" || info["state"] != "MODULE_STATE_ACTIVE" {
		t.Fatalf("module info = %v", info)
	}
	errOut := e.fails("registry", "module", "create", mod)
	t.Logf("duplicate create: %s", errOut)
	if !strings.Contains(errOut, "already exists") {
		t.Fatalf("duplicate create stderr = %q", errOut)
	}
	e.fails("registry", "module", "info", e.host+"/acme/nope")

	e.ok("registry", "module", "settings", "update", mod, "--description=forecasts", "--visibility=private", "--default-label-name=trunk")
	e.json(&info, "registry", "module", "info", mod)
	if info["default_label_name"] != "trunk" {
		t.Fatalf("module info after update = %v", info)
	}
	// The CLI does not print description or visibility; check the store.
	if m := e.module("acme", "weather"); m.Description != "forecasts" || m.Visibility != store.VisibilityPrivate {
		t.Fatalf("stored module after update = %+v", m)
	}
	e.ok("registry", "module", "deprecate", mod)
	e.json(&info, "registry", "module", "info", mod)
	if info["state"] != "MODULE_STATE_DEPRECATED" {
		t.Fatalf("module info after deprecate = %v", info)
	}
	e.ok("registry", "module", "undeprecate", mod)

	e.ok("registry", "module", "create", e.host+"/admin/mine")
	if m := e.module("admin", "mine"); m.Visibility != store.VisibilityPrivate || m.DefaultLabel != "main" {
		t.Fatalf("default module settings = %+v", m)
	}
	e.fails("registry", "module", "create", e.host+"/acme/Bad_Name")
	e.ok("registry", "module", "delete", mod, "--force")
	e.fails("registry", "module", "info", mod)
}

func TestLabelCommands(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mod := e.host + "/acme/weather"
	e.ok("registry", "module", "create", mod, "--visibility=public")

	// No labels until the first commit.
	out := e.ok("registry", "module", "label", "list", mod)
	t.Logf("empty label list: %q", out)

	// Labels need commits, which arrive with push (not implemented yet), so
	// seed commits and labels through the store.
	m := e.module("acme", "weather")
	c, err := e.st.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: "b5:00"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main", "v1"} {
		if _, err := e.st.SetLabel(ctx, store.Label{ModuleID: m.ID, Name: name, CommitID: c.ID}); err != nil {
			t.Fatal(err)
		}
	}

	out = e.ok("registry", "module", "label", "list", mod)
	t.Logf("label list: %q", out)
	if !strings.Contains(out, mod+":main") || !strings.Contains(out, mod+":v1") {
		t.Fatalf("label list = %q", out)
	}
	var info map[string]any
	e.json(&info, "registry", "module", "label", "info", mod+":v1")
	t.Logf("label info: %v", info)
	if info["name"] != "v1" || info["commit"] != c.ID {
		t.Fatalf("label info = %v", info)
	}

	e.ok("registry", "module", "label", "archive", mod+":v1")
	out = e.ok("registry", "module", "label", "list", mod)
	if strings.Contains(out, mod+":v1") {
		t.Fatalf("archived label listed: %q", out)
	}
	out = e.ok("registry", "module", "label", "list", mod, "--archive-status=archived")
	if !strings.Contains(out, mod+":v1") {
		t.Fatalf("archived labels = %q", out)
	}
	errOut := e.fails("registry", "module", "label", "archive", mod+":main")
	t.Logf("archive default label: %s", errOut)
	e.ok("registry", "module", "label", "unarchive", mod+":v1")
	out = e.ok("registry", "module", "label", "list", mod)
	if !strings.Contains(out, mod+":v1") {
		t.Fatalf("unarchived label missing: %q", out)
	}
}

// writeModule writes a v2 workspace holding one module named name.
func writeModule(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["buf.yaml"] = "version: v2\nmodules:\n  - path: .\n    name: " + name + "\n"
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// pushDir pushes dir with the CLI and returns the commit id it prints.
func (e *env) pushDir(wantErr bool, dir string, args ...string) (string, string) {
	e.t.Helper()
	out, errOut := e.runIn(dir, wantErr, append([]string{"push"}, args...)...)
	_, id, _ := strings.Cut(strings.TrimSpace(out), ":")
	_, id, _ = strings.Cut(id, ":") // host:port/owner/module:commit
	return id, errOut
}

func (e *env) runIn(dir string, wantErr bool, args ...string) (string, string) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.buf, args...) //nolint:gosec // test runs the configured buf binary
	cmd.Env = e.cliEnv
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if (err != nil) != wantErr {
		e.t.Fatalf("buf %s: err = %v, wantErr %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, wantErr, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

const weatherProto = `syntax = "proto3";
package acme.weather.v1;
message Forecast { string city = 1; }
`

func TestPush(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mod := e.host + "/acme/weather"
	e.ok("registry", "module", "create", mod, "--visibility=public")

	dir := writeModule(t, mod, map[string]string{"acme/weather/v1/weather.proto": weatherProto, "LICENSE": "Apache"})
	first, errOut := e.pushDir(false, dir)
	t.Logf("push: %q %s", first, errOut)
	if len(first) != 32 {
		t.Fatalf("push printed %q, want a commit id", first)
	}
	m := e.module("acme", "weather")
	l, err := e.st.GetLabel(ctx, m.ID, "main")
	if err != nil || l.CommitID != first {
		t.Fatalf("main = %+v, %v", l, err)
	}
	man, err := e.st.GetManifest(ctx, first)
	if err != nil || len(man) != 2 {
		t.Fatalf("manifest = %+v, %v", man, err)
	}

	// Pushing the same content again is idempotent.
	again, _ := e.pushDir(false, dir)
	if again != first {
		t.Fatalf("re-push = %q, want %q", again, first)
	}

	// A label, and the commit is readable through the CLI.
	second, _ := e.pushDir(false, dir, "--label=v1")
	if second != first {
		t.Fatalf("push --label = %q, want %q", second, first)
	}
	out := e.ok("registry", "module", "label", "list", mod)
	if !strings.Contains(out, mod+":v1") || !strings.Contains(out, mod+":main") {
		t.Fatalf("labels = %q", out)
	}
	var info map[string]any
	e.json(&info, "registry", "commit", "info", mod+":"+first)
	t.Logf("commit info: %v", info)

	// Changed content is a new commit.
	if err := os.WriteFile(filepath.Join(dir, "acme/weather/v1/weather.proto"), []byte(weatherProto+"message Extra {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, _ := e.pushDir(false, dir)
	if third == first || len(third) != 32 {
		t.Fatalf("changed push = %q", third)
	}
	if cs, err := e.st.ListCommits(ctx, m.ID); err != nil || len(cs) != 2 {
		t.Fatalf("commits = %+v, %v", cs, err)
	}

	// A module that does not exist is not created implicitly.
	other := writeModule(t, e.host+"/acme/missing", map[string]string{"a.proto": weatherProto})
	_, errOut = e.pushDir(true, other)
	t.Logf("push to missing module: %s", errOut)
}

// TestBuildAndExport builds and exports a pushed module by name, label and
// commit, and a module that depends on it.
func TestBuildAndExport(t *testing.T) {
	e := newEnv(t)
	mod := e.host + "/acme/weather"
	e.ok("registry", "module", "create", mod, "--visibility=public")
	dir := writeModule(t, mod, map[string]string{"acme/weather/v1/weather.proto": weatherProto, "LICENSE": "Apache", "README.md": "# Weather"})
	id, _ := e.pushDir(false, dir)
	if again, _ := e.pushDir(false, dir, "--label=v1"); again != id {
		t.Fatalf("push --label = %q, want %q", again, id)
	}

	// The module, its labels and its commit build to the same image.
	var images []string
	for _, ref := range []string{mod, mod + ":main", mod + ":v1", mod + ":" + id} {
		out := e.ok("build", ref, "-o", "-#format=json")
		if !strings.Contains(out, `"acme/weather/v1/weather.proto"`) || !strings.Contains(out, id) {
			t.Fatalf("build %s = %s", ref, out)
		}
		images = append(images, out)
	}
	for _, img := range images[1:] {
		if img != images[0] {
			t.Fatalf("images differ:\n%s\n%s", images[0], img)
		}
	}

	for _, ref := range []string{mod, mod + ":" + id} {
		out := filepath.Join(t.TempDir(), "export")
		e.ok("export", ref, "-o", out)
		got, err := os.ReadFile(filepath.Join(out, "acme/weather/v1/weather.proto")) //nolint:gosec // test temp dir
		if err != nil || string(got) != weatherProto {
			t.Fatalf("export %s: %q, %v", ref, got, err)
		}
	}
	e.fails("build", mod+":nope")
	e.fails("export", e.host+"/acme/missing", "-o", t.TempDir())

	// A module that imports weather: dep update resolves it, push records
	// the dependency, and build/export of the remote include it.
	app := e.host + "/acme/app"
	e.ok("registry", "module", "create", app, "--visibility=public")
	appDir := writeModule(t, app, map[string]string{"acme/app/v1/app.proto": `syntax = "proto3";
package acme.app.v1;
import "acme/weather/v1/weather.proto";
message Trip { acme.weather.v1.Forecast forecast = 1; }
`})
	if err := os.WriteFile(filepath.Join(appDir, "buf.yaml"), []byte("version: v2\nmodules:\n  - path: .\n    name: "+app+"\ndeps:\n  - "+mod+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.runIn(appDir, false, "dep", "update")
	lock, err := os.ReadFile(filepath.Join(appDir, "buf.lock")) //nolint:gosec // test temp dir
	if err != nil || !strings.Contains(string(lock), id) {
		t.Fatalf("buf.lock = %s, %v", lock, err)
	}
	e.pushDir(false, appDir)
	out := e.ok("build", app, "-o", "-#format=json")
	if !strings.Contains(out, `"acme/app/v1/app.proto"`) || !strings.Contains(out, `"acme/weather/v1/weather.proto"`) {
		t.Fatalf("build %s = %s", app, out)
	}
	exp := filepath.Join(t.TempDir(), "export")
	e.ok("export", app, "-o", exp)
	for _, p := range []string{"acme/app/v1/app.proto", "acme/weather/v1/weather.proto"} {
		if _, err := os.Stat(filepath.Join(exp, p)); err != nil {
			t.Fatalf("export %s: %v", app, err)
		}
	}
}
