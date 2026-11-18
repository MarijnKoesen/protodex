// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/marijnkoesen/protodex/internal/mirror"
)

// Generated-code compatibility test: the mirrored googleapis and
// protovalidate modules must generate byte-identical Go and TypeScript code
// to the same upstream repositories used directly. Everything a plugin
// embeds (file paths, go_package and other options, descriptors) comes from
// the mirrored files, so any alteration by the mirror shows up as a diff.
//
// It needs the network (git, go install, npm), so it only runs when
// PROTODEX_CONFORMANCE_GENCOMPAT=1 (the conformance workflow's gencompat
// job). PROTODEX_CONFORMANCE_GENCOMPAT_FAULT=go_package makes the mirror
// alter a go_package; the test must then fail.

// Plugin versions are pinned; bump them deliberately.
const (
	pluginGoGRPCVersion = "v1.5.1" // google.golang.org/grpc/cmd/protoc-gen-go-grpc
	pluginESVersion     = "2.10.0" // @bufbuild/protoc-gen-es
	// protoc-gen-go is built from the google.golang.org/protobuf version in go.mod.
)

// gencompatMirrored are the entries of the repository's mirror.yaml that
// the fixture depends on.
var gencompatMirrored = []string{"googleapis/googleapis", "bufbuild/protovalidate"}

const gencompatFixturePath = "acme/gencompat/v1"

func TestGeneratedCodeCompat(t *testing.T) {
	if os.Getenv("PROTODEX_CONFORMANCE_GENCOMPAT") != "1" {
		t.Skip("set PROTODEX_CONFORMANCE_GENCOMPAT=1 to run (needs network, go and npm)")
	}
	fault := os.Getenv("PROTODEX_CONFORMANCE_GENCOMPAT_FAULT")
	all := bufCLIs(t)
	cli := all[len(all)-1]

	cfg, err := mirror.LoadFile("../../mirror.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var sel mirror.Config
	for _, u := range cfg.Upstreams {
		for _, n := range gencompatMirrored {
			if u.Name == n {
				u.Deps = nil
				if u.Name == "googleapis/googleapis" {
					// The full googleapis tree does not build as one module
					// (google/cloud/* import protos that are not in the repo),
					// so `buf dep update` would fail; mirror a self-contained
					// subset. Upstream gets the same subset.
					u.Include = []string{"google/api/annotations.proto", "google/api/http.proto", "google/api/client.proto", "google/api/launch_stage.proto", "google/type/money.proto"}
				}
				sel.Upstreams = append(sel.Upstreams, u)
			}
		}
	}
	if len(sel.Upstreams) != len(gencompatMirrored) {
		t.Fatalf("mirror.yaml must list %v", gencompatMirrored)
	}

	plugins := buildGenPlugins(t)
	fixture := readTree(t, "testdata/gencompat")
	genYAML := "version: v2\nplugins:\n" +
		"  - local: " + plugins.goPlugin + "\n    out: gen/go\n    opt: paths=source_relative\n" +
		"  - local: " + plugins.grpc + "\n    out: gen/go\n    opt: paths=source_relative\n" +
		"  - local: " + plugins.es + "\n    out: gen/es\n    opt: target=ts\n"

	// Upstream side: the repositories cloned at the pinned refs, used
	// directly as workspace modules.
	e := newEnv(t, cli, backends[0], "")
	up := t.TempDir()
	buf := "version: v2\nmodules:\n  - path: fixture\n"
	files := map[string]string{"gen.yaml": genYAML}
	for p, c := range fixture {
		files["fixture/"+p] = c
	}
	upstreamTrees := map[string]string{}
	for _, u := range sel.Upstreams {
		dir := filepath.Join(up, "mod-"+u.Module())
		cloneUpstream(t, u, dir)
		upstreamTrees[u.Name] = dir
		buf += "  - path: mod-" + u.Module() + "\n"
	}
	files["buf.yaml"] = buf
	e.WriteFiles(up, files)
	e.OK(up, "generate", "--template", "gen.yaml", "--include-imports", "--path", "fixture/"+gencompatFixturePath)

	// Mirror side: the same pins imported into protodex, then the fixture
	// depending on the mirrored modules.
	var f mirror.Fetcher = mirror.GitFetcher{}
	if fault != "" {
		f = alteringFetcher{inner: f, fault: fault}
	}
	im := &mirror.Importer{Store: e.St, Blobs: e.Blobs, Fetcher: f}
	if _, err := im.Run(context.Background(), &sel); err != nil {
		t.Fatal(err)
	}
	var deps []string
	for _, u := range sel.Upstreams {
		deps = append(deps, e.Host+"/"+u.Name)
	}
	mfiles := map[string]string{"buf.gen.yaml": genYAML}
	for p, c := range fixture {
		mfiles[p] = c
	}
	mdir := e.Workspace("gencompat", deps, mfiles)
	e.OK(mdir, "dep", "update")
	e.OK(mdir, "generate", "--include-imports", "--path", gencompatFixturePath)

	for _, lang := range []string{"go", "es"} {
		wantFiles := readTree(t, filepath.Join(up, "gen", lang))
		gotFiles := readTree(t, filepath.Join(mdir, "gen", lang))
		if len(wantFiles) == 0 {
			t.Fatalf("no %s code was generated from upstream", lang)
		}
		if diffs := compareGenerated(wantFiles, gotFiles); len(diffs) > 0 {
			t.Errorf("mirror altered generated %s code (upstream vs mirror):\n%s", lang, strings.Join(diffs, "\n"))
		}
	}

	// The mirrored sources themselves: every upstream .proto file, same path
	// and bytes.
	for _, u := range sel.Upstreams {
		out := t.TempDir()
		e.OK(out, "export", e.Host+"/"+u.Name, "-o", "export")
		want := protoOnly(readTree(t, upstreamTrees[u.Name]))
		got := protoOnly(readTree(t, filepath.Join(out, "export")))
		if diffs := compareGenerated(want, got); len(diffs) > 0 {
			t.Errorf("mirror altered the sources of %s (upstream vs mirror):\n%s", u.Name, strings.Join(diffs, "\n"))
		}
	}
}

// alteringFetcher injects a mirror bug: a changed go_package.
type alteringFetcher struct {
	inner mirror.Fetcher
	fault string
}

func (a alteringFetcher) Fetch(ctx context.Context, url, ref string) (mirror.Snapshot, error) {
	s, err := a.inner.Fetch(ctx, url, ref)
	if err != nil || a.fault != "go_package" {
		return s, err
	}
	const name = "google/api/annotations.proto"
	if !strings.Contains(url, "googleapis") {
		return s, nil
	}
	data, err := fs.ReadFile(s.FS, name)
	if err != nil {
		s.Close()
		return mirror.Snapshot{}, err
	}
	altered := strings.Replace(string(data), "googleapis/api/annotations", "googleapis/api/annotations_altered", 1)
	if altered == string(data) {
		s.Close()
		return mirror.Snapshot{}, fmt.Errorf("fault injection: no go_package in %s", name)
	}
	s.FS = overlayFS{FS: s.FS, name: name, data: []byte(altered)}
	return s, nil
}

type overlayFS struct {
	fs.FS
	name string
	data []byte
}

func (o overlayFS) Open(name string) (fs.File, error) {
	if name == o.name {
		return fstest.MapFS{name: {Data: o.data}}.Open(name)
	}
	return o.FS.Open(name)
}

type genPlugins struct{ goPlugin, grpc, es string }

// buildGenPlugins installs the pinned local plugins into a temp directory.
func buildGenPlugins(t *testing.T) genPlugins {
	t.Helper()
	bin := t.TempDir()
	run := func(dir, name string, env []string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...) //nolint:gosec // fixed tool invocations
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
	}
	run(".", "go", nil, "build", "-o", filepath.Join(bin, "protoc-gen-go"), "google.golang.org/protobuf/cmd/protoc-gen-go")
	run(bin, "go", []string{"GOBIN=" + bin, "GOFLAGS=-mod=mod"}, "install", "google.golang.org/grpc/cmd/protoc-gen-go-grpc@"+pluginGoGRPCVersion)
	npm := t.TempDir()
	run(npm, "npm", nil, "install", "--no-audit", "--no-fund", "--ignore-scripts", "--prefix", npm, "@bufbuild/protoc-gen-es@"+pluginESVersion)
	return genPlugins{
		goPlugin: filepath.Join(bin, "protoc-gen-go"),
		grpc:     filepath.Join(bin, "protoc-gen-go-grpc"),
		es:       filepath.Join(npm, "node_modules", ".bin", "protoc-gen-es"),
	}
}

// cloneUpstream checks the pinned ref of u out of git and copies the .proto
// files the mirror config selects (roots, include) into dst, laid out
// relative to the root like the mirrored module. It does not use the
// mirror's own code.
func cloneUpstream(t *testing.T, u mirror.Upstream, dst string) {
	t.Helper()
	repo := t.TempDir()
	ref := u.Ref
	if !regexp.MustCompile(`^[0-9a-f]{40,64}$`).MatchString(ref) {
		ref = "refs/tags/" + ref
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "core.autocrlf=false", "-c", "core.symlinks=false"}, args...)...) //nolint:gosec // test
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "attributes"), []byte("* -text -eol -filter -ident -export-subst\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("fetch", "-q", "--depth", "1", "--no-tags", "--", u.URL, ref)
	git("checkout", "-q", "--detach", "FETCH_HEAD")
	roots := u.Roots
	if len(roots) == 0 {
		roots = []string{"."}
	}
	for _, root := range roots {
		base := filepath.Join(repo, filepath.FromSlash(root))
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == ".git" {
				return fs.SkipDir
			}
			if !d.Type().IsRegular() || path.Ext(p) != ".proto" {
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			rel = filepath.ToSlash(rel)
			if !hasPrefixIn(rel, u.Include) {
				return nil
			}
			data, err := os.ReadFile(p) //nolint:gosec // temp checkout
			if err != nil {
				return err
			}
			out := filepath.Join(dst, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil { //nolint:gosec // test temp dir
				return err
			}
			return os.WriteFile(out, data, 0o600) //nolint:gosec // test temp dir
		})
		if err != nil {
			t.Fatalf("copy %s: %v", u.Name, err)
		}
	}
}

func hasPrefixIn(p string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, pre := range prefixes {
		pre = strings.TrimSuffix(pre, "/")
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// readTree returns every file under dir by slash-separated relative path.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == dir {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // test temp dir
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return files
}

func protoOnly(files map[string]string) map[string]string {
	out := map[string]string{}
	for p, c := range files {
		if path.Ext(p) == ".proto" {
			out[p] = c
		}
	}
	return out
}

// headerVersionLine matches the generator and compiler version lines that
// may legitimately differ between runs (protoc-gen-go "// \tprotoc-gen-go
// vX", "// \tprotoc vX", protoc-gen-es "// @generated by protoc-gen-es vX").
var headerVersionLine = regexp.MustCompile(`^// (\s*protoc(-gen-[a-z-]+)?\s+\S+.*|@generated by protoc-gen-\S+ \S+.*|versions:)$`)

// normalizeGenerated blanks the version lines of the file header.
func normalizeGenerated(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if i < 20 && headerVersionLine.MatchString(l) {
			lines[i] = "// <version line>"
		}
	}
	return strings.Join(lines, "\n")
}

// compareGenerated returns a description of every difference between two
// trees (missing, extra or changed files), ignoring version header lines.
func compareGenerated(want, got map[string]string) []string {
	var diffs []string
	names := map[string]bool{}
	for p := range want {
		names[p] = true
	}
	for p := range got {
		names[p] = true
	}
	var sorted []string
	for p := range names {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		w, wok := want[p]
		g, gok := got[p]
		switch {
		case !gok:
			diffs = append(diffs, "missing from mirror: "+p)
		case !wok:
			diffs = append(diffs, "only in mirror: "+p)
		case normalizeGenerated(w) != normalizeGenerated(g):
			diffs = append(diffs, "differs: "+p+"\n"+firstDiff(normalizeGenerated(w), normalizeGenerated(g)))
		}
	}
	return diffs
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < max(len(al), len(bl)); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("  line %d:\n    upstream: %s\n    mirror:   %s", i+1, truncate(x), truncate(y))
		}
	}
	return ""
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func TestCompareGenerated(t *testing.T) {
	hdr := "// Code generated by protoc-gen-go. DO NOT EDIT.\n// versions:\n// \tprotoc-gen-go v1.36.12\n// \tprotoc        (unknown)\n// source: a.proto\n"
	a := map[string]string{"a.pb.go": hdr + "package a\n", "b.pb.go": "x"}
	same := map[string]string{
		"a.pb.go": strings.ReplaceAll(strings.ReplaceAll(hdr, "v1.36.12", "v1.99.0"), "(unknown)", "v5.0.0") + "package a\n",
		"b.pb.go": "x",
	}
	if d := compareGenerated(a, same); len(d) != 0 {
		t.Fatalf("version header changes must be allowed, got %v", d)
	}
	altered := map[string]string{"a.pb.go": hdr + "package a // go_package altered\n", "b.pb.go": "x", "c.pb.go": "y"}
	d := strings.Join(compareGenerated(a, altered), "\n")
	for _, want := range []string{"differs: a.pb.go", "only in mirror: c.pb.go"} {
		if !strings.Contains(d, want) {
			t.Fatalf("missing %q in %q", want, d)
		}
	}
	if d := compareGenerated(a, map[string]string{"a.pb.go": a["a.pb.go"]}); len(d) != 1 || !strings.Contains(d[0], "missing from mirror: b.pb.go") {
		t.Fatalf("missing file not reported: %v", d)
	}
}
