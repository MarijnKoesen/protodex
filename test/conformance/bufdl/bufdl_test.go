// SPDX-License-Identifier: Apache-2.0

package bufdl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestGet(t *testing.T) {
	good := []byte("#!/bin/sh\necho 9.9.9\n")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/v9.9.9/buf-Linux-x86_64":
			_, _ = w.Write(good)
		case "/v9.9.8/buf-Linux-x86_64":
			_, _ = w.Write([]byte("tampered"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	d := &Downloader{
		Dir: t.TempDir(), BaseURL: srv.URL, GOOS: "linux", GOARCH: "amd64",
		Checksums: Checksums{
			"9.9.9": {"buf-Linux-x86_64": sum(good)},
			"9.9.8": {"buf-Linux-x86_64": sum(good)},
		},
	}
	ctx := context.Background()

	p, err := d.Get(ctx, "v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(p); err != nil || string(got) != string(good) { //nolint:gosec // test temp dir
		t.Fatalf("binary = %q, %v", got, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode()&0o100 == 0 {
		t.Fatalf("binary not executable: %v, %v", fi.Mode(), err)
	}
	// Cached and verified: no second download.
	if _, err := d.Get(ctx, "9.9.9"); err != nil || hits.Load() != 1 {
		t.Fatalf("cached get: err %v, %d downloads", err, hits.Load())
	}
	// A corrupted cache entry is replaced.
	if err := os.WriteFile(p, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Get(ctx, "9.9.9"); err != nil || hits.Load() != 2 {
		t.Fatalf("re-download: err %v, %d downloads", err, hits.Load())
	}

	// A checksum mismatch is refused and leaves nothing behind.
	_, err = d.Get(ctx, "9.9.8")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered download: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.Dir, "9.9.8", "buf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tampered binary kept: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(d.Dir, "9.9.8"))
	if len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}

	// Unpinned versions and platforms are refused without downloading.
	before := hits.Load()
	if _, err := d.Get(ctx, "1.0.0"); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("unpinned version: %v", err)
	}
	d.GOARCH = "arm64"
	if _, err := d.Get(ctx, "9.9.9"); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("unpinned platform: %v", err)
	}
	if hits.Load() != before {
		t.Fatal("unpinned get downloaded")
	}
}

func TestFetchReleaseChecksums(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v9.9.9/sha256.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(strings.Repeat("ab", 32) + "  buf-Linux-x86_64\n" +
			strings.Repeat("cd", 32) + "  buf-Linux-x86_64.tar.gz\n" +
			strings.Repeat("EF", 32) + "  buf-Darwin-arm64\n"))
	}))
	defer srv.Close()
	got, err := (&Downloader{BaseURL: srv.URL}).FetchReleaseChecksums(context.Background(), "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["buf-Linux-x86_64"] != strings.Repeat("ab", 32) || got["buf-Darwin-arm64"] != strings.Repeat("ef", 32) {
		t.Fatalf("checksums = %v", got)
	}
	if _, err := (&Downloader{BaseURL: srv.URL}).FetchReleaseChecksums(context.Background(), "1.0.0"); err == nil {
		t.Fatal("missing release: want error")
	}
}

func TestAsset(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "buf-Linux-x86_64"},
		{"linux", "arm64", "buf-Linux-aarch64"},
		{"darwin", "amd64", "buf-Darwin-x86_64"},
		{"darwin", "arm64", "buf-Darwin-arm64"},
	} {
		if got, err := Asset(tc.goos, tc.goarch); err != nil || got != tc.want {
			t.Errorf("Asset(%s, %s) = %q, %v", tc.goos, tc.goarch, got, err)
		}
	}
	if _, err := Asset("windows", "amd64"); err == nil {
		t.Error("windows: want error")
	}
	if _, err := Asset("linux", "386"); err == nil {
		t.Error("386: want error")
	}
}

func TestChecksumsRoundTrip(t *testing.T) {
	c, err := ParseChecksums("# comment\n\n1.10.0 " + strings.Repeat("aa", 32) + " buf-Linux-x86_64\n1.9.0 " + strings.Repeat("bb", 32) + " buf-Linux-x86_64\n")
	if err != nil {
		t.Fatal(err)
	}
	if v := c.Versions(); len(v) != 2 || v[0] != "1.9.0" {
		t.Fatalf("versions = %v (want numeric order)", v)
	}
	again, err := ParseChecksums(c.Format())
	if err != nil || again.Format() != c.Format() {
		t.Fatalf("round trip: %v\n%s", err, again.Format())
	}
	for _, bad := range []string{"1.0.0 abc buf-Linux-x86_64", "1.0.0 " + strings.Repeat("aa", 32)} {
		if _, err := ParseChecksums(bad); err == nil {
			t.Errorf("ParseChecksums(%q): want error", bad)
		}
	}
}

func TestPinnedAndSupported(t *testing.T) {
	all, err := Supported(false)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := Supported(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 || len(pr) == 0 || len(pr) > len(all) {
		t.Fatalf("supported = %v, pr = %v", all, pr)
	}
	// The PR subset includes the minimum and latest supported versions.
	if pr[0] != all[0] || pr[len(pr)-1] != all[len(all)-1] {
		t.Fatalf("pr subset %v must include min %s and latest %s", pr, all[0], all[len(all)-1])
	}
	for i := 1; i < len(all); i++ {
		if CompareVersions(all[i-1], all[i]) >= 0 {
			t.Fatalf("versions.txt not ascending: %v", all)
		}
	}
	pinned, err := Pinned()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range all {
		for _, a := range PinnedAssets {
			if pinned[v][a] == "" {
				t.Errorf("no checksum pinned for %s %s", v, a)
			}
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.32.2", "1.32.2", 0},
		{"v1.32.2", "1.32.2", 0},
		{"1.9.0", "1.10.0", -1},
		{"1.40", "1.40.0", 0},
		{"1.75.0", "1.57.2", 1},
		{"1.32.0-beta.1", "1.32.0", 0},
	} {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
