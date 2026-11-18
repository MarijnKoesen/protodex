// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	ownerv1 "github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1"
	"github.com/marijnkoesen/protodex/internal/store"
)

// The remote backend runs the suite against an already deployed protodex
// (the kind e2e in deploy/e2e) instead of an in-process server:
//
//	PROTODEX_CONFORMANCE_REMOTE=http://127.0.0.1:30080  base URL of the server
//	PROTODEX_CONFORMANCE_REMOTE_TOKEN=pdx_...           token of user "admin"
//
// The server must have user "admin" (owning that token) and organization
// "acme", e.g. from `protodex bootstrap -org acme`. The buf CLI only speaks
// TLS, so each Env puts a local HTTPS reverse proxy with a test certificate in
// front of the server. Module names get a random suffix so scenarios can share
// the server, and objects are read through the Connect API instead of the
// store.
func remoteURL() string   { return os.Getenv("PROTODEX_CONFORMANCE_REMOTE") }
func remoteToken() string { return os.Getenv("PROTODEX_CONFORMANCE_REMOTE_TOKEN") }

func remoteSkip() string {
	switch {
	case remoteURL() == "":
		return "PROTODEX_CONFORMANCE_REMOTE not set (no deployed server to test)"
	case remoteToken() == "":
		return "PROTODEX_CONFORMANCE_REMOTE_TOKEN not set"
	}
	return ""
}

var remoteBackend = Backend{Name: "remote", Skip: remoteSkip, Remote: true}

func init() { registerBackend(remoteBackend) }

// newRemoteEnv is newEnv for the remote backend.
func newRemoteEnv(t T, cli *bufCLI, fault string) *Env {
	t.Helper()
	if fault != "" {
		t.Skipf("fault injection needs the in-process server")
	}
	target, err := url.Parse(remoteURL())
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		t.Fatalf("PROTODEX_CONFORMANCE_REMOTE=%q: want an http(s)://host:port URL", remoteURL())
	}
	dir := t.TempDir()
	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("proxy request log (remote %s, buf %s):\n%s", target, cli.Version, logBuf.String())
		}
	})

	tr := http.DefaultTransport.(*http.Transport).Clone()
	if target.Scheme == "http" {
		// protodex serves h2c; speak HTTP/2 end to end like a gateway would.
		var p http.Protocols
		p.SetUnencryptedHTTP2(true)
		tr.Protocols = &p
	}
	proxy := &httputil.ReverseProxy{
		Rewrite:       func(r *httputil.ProxyRequest) { r.SetURL(target) },
		Transport:     tr,
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	ts := httptest.NewUnstartedServer(errorBodies(logger, proxy))
	ts.EnableHTTP2 = true
	ts.Config.ErrorLog = slog.NewLogLogger(logger.Handler(), slog.LevelWarn)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	t.Cleanup(tr.CloseIdleConnections)

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	e := &Env{
		t: t, cli: cli, Host: strings.TrimPrefix(ts.URL, "https://"), Token: remoteToken(),
		home: filepath.Join(dir, "home"), cache: filepath.Join(dir, "cache"),
		certFile: filepath.Join(dir, "cert.pem"), log: logBuf, logger: logger,
		api: newRemoteAPI(target.String(), remoteToken(), tr), suffix: hex.EncodeToString(suffix),
	}
	if err := os.WriteFile(e.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.Mkdir(e.home, 0o700); err != nil {
		t.Fatalf("home: %v", err)
	}
	return e
}

// remoteAPI is a Connect client for the registry objects scenarios inspect.
type remoteAPI struct {
	base    string
	http    *http.Client
	proxy   *http.Transport // the CLI proxy's transport, reset after outages
	modules modulev1connect.ModuleServiceClient
	labels  modulev1connect.LabelServiceClient
	commits modulev1connect.CommitServiceClient
}

func newRemoteAPI(base, token string, proxy *http.Transport) *remoteAPI {
	base = strings.TrimSuffix(base, "/")
	hc := &http.Client{Timeout: 30 * time.Second}
	auth := connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}))
	return &remoteAPI{
		base: base, http: hc, proxy: proxy,
		modules: modulev1connect.NewModuleServiceClient(hc, base, auth),
		labels:  modulev1connect.NewLabelServiceClient(hc, base, auth),
		commits: modulev1connect.NewCommitServiceClient(hc, base, auth),
	}
}

func (a *remoteAPI) getModule(ctx context.Context, owner, name string) (store.Module, error) {
	resp, err := a.modules.GetModules(ctx, connect.NewRequest(&modulev1.GetModulesRequest{ModuleRefs: []*modulev1.ModuleRef{
		{Value: &modulev1.ModuleRef_Name_{Name: &modulev1.ModuleRef_Name{Owner: owner, Module: name}}},
	}}))
	if err != nil {
		return store.Module{}, err
	}
	if len(resp.Msg.GetModules()) != 1 {
		return store.Module{}, fmt.Errorf("GetModules %s/%s returned %d modules", owner, name, len(resp.Msg.GetModules()))
	}
	m := resp.Msg.GetModules()[0]
	vis := map[modulev1.ModuleVisibility]string{
		modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC:  store.VisibilityPublic,
		modulev1.ModuleVisibility_MODULE_VISIBILITY_PRIVATE: store.VisibilityPrivate,
	}[m.GetVisibility()]
	return store.Module{
		ID: m.GetId(), OwnerID: m.GetOwnerId(), Name: m.GetName(), Visibility: vis,
		DefaultLabel: m.GetDefaultLabelName(), Description: m.GetDescription(), URL: m.GetUrl(),
		Deprecated: m.GetState() == modulev1.ModuleState_MODULE_STATE_DEPRECATED,
		CreatedAt:  m.GetCreateTime().AsTime(), UpdatedAt: m.GetUpdateTime().AsTime(),
	}, nil
}

func (a *remoteAPI) getLabelCommit(ctx context.Context, owner, name, label string) (store.Commit, error) {
	lr, err := a.labels.GetLabels(ctx, connect.NewRequest(&modulev1.GetLabelsRequest{LabelRefs: []*modulev1.LabelRef{
		{Value: &modulev1.LabelRef_Name_{Name: &modulev1.LabelRef_Name{Owner: owner, Module: name, Label: label}}},
	}}))
	if err != nil {
		return store.Commit{}, err
	}
	if len(lr.Msg.GetLabels()) != 1 {
		return store.Commit{}, fmt.Errorf("GetLabels %s/%s:%s returned %d labels", owner, name, label, len(lr.Msg.GetLabels()))
	}
	id := lr.Msg.GetLabels()[0].GetCommitId()
	cr, err := a.commits.GetCommits(ctx, connect.NewRequest(&modulev1.GetCommitsRequest{ResourceRefs: []*modulev1.ResourceRef{
		{Value: &modulev1.ResourceRef_Id{Id: id}},
	}}))
	if err != nil {
		return store.Commit{}, err
	}
	if len(cr.Msg.GetCommits()) != 1 {
		return store.Commit{}, fmt.Errorf("GetCommits %s returned %d commits", id, len(cr.Msg.GetCommits()))
	}
	c := cr.Msg.GetCommits()[0]
	digest := ""
	if d := c.GetDigest(); d != nil && d.GetType() == modulev1.DigestType_DIGEST_TYPE_B5 {
		digest = "b5:" + hex.EncodeToString(d.GetValue())
	}
	return store.Commit{ID: c.GetId(), ModuleID: c.GetModuleId(), Digest: digest, CreatedAt: c.GetCreateTime().AsTime(),
		SourceControlURL: c.GetSourceControlUrl()}, nil
}

func (a *remoteAPI) module(t T, owner, name string) store.Module {
	t.Helper()
	m, err := a.getModule(context.Background(), owner, name)
	if err != nil {
		t.Fatalf("get module %s/%s: %v", owner, name, err)
	}
	return m
}

func (a *remoteAPI) labelCommit(t T, owner, name, label string) store.Commit {
	t.Helper()
	c, err := a.getLabelCommit(context.Background(), owner, name, label)
	if err != nil {
		t.Fatalf("get label %s/%s:%s: %v", owner, name, label, err)
	}
	return c
}

func (a *remoteAPI) createModule(t T, owner, name string) {
	t.Helper()
	_, err := a.modules.CreateModules(context.Background(), connect.NewRequest(&modulev1.CreateModulesRequest{Values: []*modulev1.CreateModulesRequest_Value{{
		OwnerRef:   &ownerv1.OwnerRef{Value: &ownerv1.OwnerRef_Name{Name: owner}},
		Name:       name,
		Visibility: modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC,
	}}}))
	if err != nil {
		t.Fatalf("create module %s/%s: %v", owner, name, err)
	}
}

// waitReady polls until /readyz answers 200 and probe succeeds through the
// API, or timeout passes. A probe that fails with NotFound, Unauthenticated
// or PermissionDenied fails the test at once: the server is up but lost data.
// On success it drops the pooled connections, which may point at the server
// instance that went away.
func (a *remoteAPI) waitReady(t T, timeout time.Duration, probe func(context.Context) error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		last = a.readyOnce(ctx, probe)
		cancel()
		switch connect.CodeOf(last) {
		case connect.CodeNotFound, connect.CodeUnauthenticated, connect.CodePermissionDenied:
			// The server is up but has lost data (or the token): retrying
			// will not help.
			t.Fatalf("server is up but the probe failed: %v", last)
		}
		if last == nil {
			t.Logf("server ready after %d attempt(s)", attempt)
			a.http.CloseIdleConnections()
			a.proxy.CloseIdleConnections()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server not ready after %s: %v", timeout, last)
		}
		time.Sleep(2 * time.Second) // poll interval
	}
}

func (a *remoteAPI) readyOnce(ctx context.Context, probe func(context.Context) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/readyz: %s", resp.Status)
	}
	return probe(ctx)
}

// registryState is what a disruption must not lose.
type registryState struct {
	Modules map[string]store.Module
	Labels  map[string]store.Commit // "<module>:<label>" -> commit
}

var disruptionLabels = map[string][]string{"weather": {"main", "v1"}, "app": {"main"}}

func snapshot(ctx context.Context, e *Env) (registryState, error) {
	s := registryState{Modules: map[string]store.Module{}, Labels: map[string]store.Commit{}}
	for name, labels := range disruptionLabels {
		m, err := e.api.getModule(ctx, "acme", e.Name(name))
		if err != nil {
			return s, err
		}
		s.Modules[name] = m
		for _, l := range labels {
			c, err := e.api.getLabelCommit(ctx, "acme", e.Name(name), l)
			if err != nil {
				return s, err
			}
			s.Labels[name+":"+l] = c
		}
	}
	return s, nil
}

// TestDisruption is the kind e2e's lifecycle test (deploy/e2e): it pushes
// modules, commits and labels to the remote server, runs the shell command
// in PROTODEX_E2E_DISRUPT (kill the pod, wipe the volume and restore from the
// Litestream replica, upgrade the release, ...), waits for the server to come
// back and checks that nothing was lost, that the same CLI configuration
// (token, cache) keeps working and that the server accepts writes again.
//
// PROTODEX_E2E_CHECK_CONTENT=false skips downloading module content after the
// disruption (blobs live on the volume until the S3 blob store, #8, lands, so
// a volume restore brings back metadata only).
func TestDisruption(t *testing.T) {
	disrupt := os.Getenv("PROTODEX_E2E_DISRUPT")
	if disrupt == "" {
		t.Skip("PROTODEX_E2E_DISRUPT not set (run through deploy/e2e/e2e.sh)")
	}
	if reason := remoteSkip(); reason != "" {
		t.Skip(reason)
	}
	all := bufCLIs(t)
	cli := all[0]
	for _, c := range all {
		if c.atLeast(cli.Version) {
			cli = c
		}
	}
	e := newEnv(t, cli, remoteBackend, "")
	if needsLabels(cli) != "" {
		t.Fatalf("buf %s has no label commands; use a newer CLI", cli.Version)
	}

	// Seed: weather with two commits (v1 on the first, main on the second)
	// and app depending on it.
	first, wdir := pushWeather(e, "v1")
	e.WriteFiles(wdir, map[string]string{weatherPath: weatherProto + "\n// Extra is new.\nmessage Extra {}\n"})
	second := e.Push(wdir)
	adir := appWorkspace(e)
	e.CreateModule("app")
	e.Push(adir)
	ctx := context.Background()
	before, err := snapshot(ctx, e)
	if err != nil {
		t.Fatalf("snapshot before: %v", err)
	}
	if before.Labels["weather:v1"].ID != first || before.Labels["weather:main"].ID != second {
		t.Fatalf("seeded labels = %+v, want v1=%s main=%s", before.Labels, first, second)
	}
	t.Logf("seeded acme/%s (v1=%s, main=%s) and acme/%s", e.Name("weather"), first, second, e.Name("app"))

	// Disrupt.
	dctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(dctx, "sh", "-c", disrupt) //nolint:gosec // command chosen by the e2e driver
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Env = append(os.Environ(), "PROTODEX_E2E_MODULE=acme/"+e.Name("weather"))
	t.Logf("$ %s", disrupt)
	if err := cmd.Run(); err != nil {
		t.Fatalf("disruption %q: %v", disrupt, err)
	}

	// Recover: the server answers again with the seeded objects.
	e.api.waitReady(t, 5*time.Minute, func(ctx context.Context) error {
		_, err := e.api.getModule(ctx, "acme", e.Name("weather"))
		return err
	})
	after, err := snapshot(ctx, e)
	if err != nil {
		t.Fatalf("snapshot after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("registry state changed across the disruption:\nbefore: %+v\nafter:  %+v", before, after)
	}

	// The CLI, with its existing configuration, sees the same labels.
	mod := e.Mod("weather")
	out := e.OK("", append(labelCmd(cli, "list"), mod)...)
	mustContain(t, "label list", out, mod+":main", mod+":v1")

	if os.Getenv("PROTODEX_E2E_CHECK_CONTENT") != "false" {
		exp := filepath.Join(t.TempDir(), "export")
		e.OK("", "export", mod+":v1", "-o", exp)
		if got := e.ReadFile(exp, weatherPath); got != weatherProto {
			t.Fatalf("export %s:v1: %s = %q", mod, weatherPath, got)
		}
		e.ClearCache()
		e.OK(adir, "build")
		img := e.OK("", "build", e.Mod("app"), "-o", "-#format=json")
		mustContain(t, "remote app image", img, appPath, weatherPath)
	} else {
		t.Logf("PROTODEX_E2E_CHECK_CONTENT=false: not downloading module content")
	}

	// Writes work again.
	e.WriteFiles(wdir, map[string]string{weatherPath: weatherProto + "\n// After is pushed after the disruption.\nmessage After {}\n"})
	third := e.Push(wdir)
	if c := e.LabelCommit("weather", "main"); c.ID != third {
		t.Fatalf("main after a post-disruption push = %s, want %s", c.ID, third)
	}
	if c := e.LabelCommit("weather", "v1"); c.ID != first {
		t.Fatalf("v1 moved to %s", c.ID)
	}
}
