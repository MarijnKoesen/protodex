// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/marijnkoesen/protodex/internal/blob"
	"github.com/marijnkoesen/protodex/internal/blob/fsblob"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

type fakeFetcher struct {
	trees map[string]fstest.MapFS // by url
	sha   string
	calls int
}

func (f *fakeFetcher) Fetch(_ context.Context, url, _ string) (Snapshot, error) {
	f.calls++
	return Snapshot{Commit: f.sha, FS: f.trees[url], Close: func() {}}, nil
}

const sha1 = "1111111111111111111111111111111111111111"

func newImporter(t *testing.T, f Fetcher) *Importer {
	t.Helper()
	dir := t.TempDir()
	st, err := sqlite.Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	blobs, err := fsblob.New(filepath.Join(dir, "blobs"), 0)
	if err != nil {
		t.Fatal(err)
	}
	return &Importer{Store: st, Blobs: blobs, Fetcher: f}
}

const testConfig = `
upstreams:
  - name: bufbuild/protovalidate
    url: https://example.com/protovalidate.git
    ref: v1.0.0
    license: Apache-2.0
    roots: [proto/protovalidate]
    deps: [googleapis/googleapis]
  - name: googleapis/googleapis
    url: https://example.com/googleapis.git
    ref: ` + sha1 + `
    license: Apache-2.0
    include: [google]
`

func trees() map[string]fstest.MapFS {
	return map[string]fstest.MapFS{
		"https://example.com/googleapis.git": {
			"LICENSE":                      {Data: []byte("Apache")},
			"google/api/annotations.proto": {Data: []byte("syntax = \"proto3\";\noption go_package = \"google.golang.org/genproto/googleapis/api/annotations;annotations\";\n")},
			"google/api/BUILD.bazel":       {Data: []byte("x")},
			"grafeas/v1/x.proto":           {Data: []byte("excluded")},
		},
		"https://example.com/protovalidate.git": {
			"LICENSE": {Data: []byte("Apache pv")},
			"proto/protovalidate/buf/validate/v.proto": {Data: []byte("syntax = \"proto3\";\n")},
			"proto/other/ignored.proto":                {Data: []byte("ignored")},
		},
	}
}

func TestImportPreservesContentAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	ff := &fakeFetcher{trees: trees(), sha: sha1}
	im := newImporter(t, ff)
	cfg, err := Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	res, err := im.Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Name != "googleapis/googleapis" || !res[0].Created || !res[1].Created {
		t.Fatalf("results (deps first): %+v", res)
	}
	st := im.Store
	o, err := st.GetOwner(ctx, "googleapis")
	if err != nil || !o.Mirror {
		t.Fatalf("mirror owner: %+v %v", o, err)
	}
	m, _ := st.GetModule(ctx, o.ID, "googleapis")
	c := res[0].Commit
	man, err := st.GetManifest(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range man {
		paths = append(paths, e.Path)
	}
	if !slices.Equal(paths, []string{"LICENSE", "google/api/annotations.proto"}) {
		t.Fatalf("paths = %v", paths)
	}
	b, _ := st.GetBlob(ctx, man[1].BlobDigest)
	if b.Size != int64(len(trees()["https://example.com/googleapis.git"]["google/api/annotations.proto"].Data)) {
		t.Fatalf("blob size %d", b.Size)
	}
	for _, l := range []string{"main"} {
		lab, err := st.GetLabel(ctx, m.ID, l)
		if err != nil || lab.CommitID != c.ID {
			t.Fatalf("label %s: %+v %v", l, lab, err)
		}
	}
	mc, err := st.GetMirrorCommit(ctx, c.ID)
	if err != nil || mc.UpstreamCommit != sha1 || mc.License != "Apache-2.0" || mc.ImportedAt.IsZero() {
		t.Fatalf("provenance: %+v %v", mc, err)
	}
	if got, _ := st.GetCommitByID(ctx, c.ID); got.SourceControlURL != "https://example.com/googleapis" {
		t.Fatalf("source url %q", got.SourceControlURL)
	}

	// protovalidate: tag label, dependency edge, root-relative paths.
	po, _ := st.GetOwner(ctx, "bufbuild")
	pm, _ := st.GetModule(ctx, po.ID, "protovalidate")
	if l, err := st.GetLabel(ctx, pm.ID, "v1.0.0"); err != nil || l.CommitID != res[1].Commit.ID {
		t.Fatalf("tag label: %+v %v", l, err)
	}
	deps, _ := st.ListCommitDeps(ctx, res[1].Commit.ID)
	if len(deps) != 1 || deps[0].DepOwner != "googleapis" || deps[0].DepCommitID != c.ID {
		t.Fatalf("deps: %+v", deps)
	}
	pman, _ := st.GetManifest(ctx, res[1].Commit.ID)
	if pman[1].Path != "buf/validate/v.proto" {
		t.Fatalf("manifest: %+v", pman)
	}

	// Re-run: no-op.
	again, err := im.Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range again {
		if r.Created || len(r.LabelsMoved) != 0 || r.Commit.ID != res[i].Commit.ID {
			t.Fatalf("re-run not a no-op: %+v", r)
		}
	}
	if cs, _ := st.ListCommits(ctx, m.ID); len(cs) != 1 {
		t.Fatalf("commits after re-run: %d", len(cs))
	}
}

func TestImportNewContentMakesNewCommit(t *testing.T) {
	ctx := context.Background()
	ff := &fakeFetcher{trees: trees(), sha: sha1}
	im := newImporter(t, ff)
	cfg, _ := Parse([]byte(testConfig))
	first, err := im.Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ff.trees["https://example.com/googleapis.git"]["google/api/annotations.proto"] = &fstest.MapFile{Data: []byte("changed")}
	second, err := im.Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !second[0].Created || second[0].Commit.ID == first[0].Commit.ID || !slices.Contains(second[0].LabelsMoved, "main") {
		t.Fatalf("second: %+v", second[0])
	}
	// protovalidate depends on the changed commit, so it is re-imported too.
	if !second[1].Created {
		t.Fatalf("dependent not re-imported: %+v", second[1])
	}
}

func TestPinnedCommitMustMatch(t *testing.T) {
	ff := &fakeFetcher{trees: trees(), sha: "2222222222222222222222222222222222222222"}
	im := newImporter(t, ff)
	cfg, _ := Parse([]byte(testConfig))
	if _, err := im.Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "resolved to commit") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	base := func(mod string) string {
		return "upstreams:\n  - name: a/b\n    url: https://x.test/r.git\n    ref: v1\n    license: Apache-2.0\n" + mod
	}
	tests := map[string]string{
		"license not approved": strings.Replace(base(""), "Apache-2.0", "GPL-3.0-only", 1),
		"bad name":             strings.Replace(base(""), "a/b", "ab", 1),
		"branch ref":           strings.Replace(base(""), "ref: v1", "ref: refs/heads/main", 1),
		"flag-like url":        strings.Replace(base(""), "https://x.test/r.git", "--upload-pack=x", 1),
		"escaping root":        base("    roots: [../x]\n"),
		"unknown dep":          base("    deps: [c/d]\n"),
		"unknown field":        base("    bogus: 1\n"),
		"cycle":                base("    deps: [c/d]\n") + "  - name: c/d\n    url: https://x.test/s.git\n    ref: v1\n    license: MIT\n    deps: [a/b]\n",
		"empty":                "upstreams: []\n",
	}
	for name, y := range tests {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGitFetcher(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command( //nolint:gosec // test runs git
			"git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, p)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, p), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitattributes", "*.proto text eol=crlf\n")
	write("LICENSE", "Apache")
	write("google/api/annotations.proto", "syntax = \"proto3\";\n")
	git("add", ".")
	git("commit", "-q", "-m", "one")
	git("tag", "v1")
	want := git("rev-parse", "HEAD")
	write("google/api/annotations.proto", "syntax = \"proto3\";\n// two\n")
	git("commit", "-qam", "two")

	cfg := "upstreams:\n  - name: googleapis/googleapis\n    url: file://" + repo + "\n    ref: v1\n    license: Apache-2.0\n"
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	im := newImporter(t, GitFetcher{AllowFile: true})
	res, err := im.Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Upstream != want {
		t.Fatalf("resolved %s, want %s", res[0].Upstream, want)
	}
	man, _ := im.Store.GetManifest(context.Background(), res[0].Commit.ID)
	d, err := blob.ParseDigest(man[1].BlobDigest)
	if err != nil {
		t.Fatal(err)
	}
	b, err := im.Blobs.Get(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(b)
	_ = b.Close()
	if string(got) != "syntax = \"proto3\";\n" { // the pinned tag, not the later commit
		t.Fatalf("content = %q", got)
	}

	// A branch is not a tag pin, even though a ref of that name exists.
	branch := git("rev-parse", "--abbrev-ref", "HEAD")
	if _, err := (GitFetcher{AllowFile: true}).Fetch(context.Background(), "file://"+repo, branch); err == nil {
		t.Fatal("branch name accepted as a tag pin")
	}

	// file:// is refused unless explicitly allowed.
	if _, err := (GitFetcher{}).Fetch(context.Background(), "file://"+repo, "v1"); err == nil {
		t.Fatal("file URL accepted without AllowFile")
	}
}
