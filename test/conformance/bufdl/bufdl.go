// SPDX-License-Identifier: Apache-2.0

// Package bufdl downloads released buf CLI binaries from GitHub and verifies
// them against SHA-256 checksums pinned in checksums.txt, so the conformance
// suite never runs an unverified binary.
//
// checksums.txt lines are "<version> <sha256> <asset>", for example
// "1.57.0 4a1e...  buf-Linux-x86_64". Add a version with
// `go run ./test/conformance/cmd/fetchbuf pin <version>` and review the diff.
package bufdl

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed" // checksums.txt
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

//go:embed checksums.txt
var pinnedFile string

// DefaultBaseURL is where release assets are fetched from;
// <base>/v<version>/<asset>.
const DefaultBaseURL = "https://github.com/bufbuild/buf/releases/download"

// Asset returns the release asset name of the buf binary for goos/goarch,
// e.g. "buf-Linux-x86_64".
func Asset(goos, goarch string) (string, error) {
	var o string
	switch goos {
	case "linux":
		o = "Linux"
	case "darwin":
		o = "Darwin"
	default:
		return "", fmt.Errorf("bufdl: unsupported OS %q", goos)
	}
	var a string
	switch {
	case goarch == "amd64":
		a = "x86_64"
	case goarch == "arm64" && goos == "linux":
		a = "aarch64"
	case goarch == "arm64":
		a = "arm64"
	default:
		return "", fmt.Errorf("bufdl: unsupported architecture %q", goarch)
	}
	return "buf-" + o + "-" + a, nil
}

// PinnedAssets lists the assets pinned for each version.
var PinnedAssets = []string{"buf-Linux-x86_64", "buf-Linux-aarch64", "buf-Darwin-x86_64", "buf-Darwin-arm64"}

// Checksums maps version → asset → lowercase hex SHA-256.
type Checksums map[string]map[string]string

// Pinned returns the checksums embedded from checksums.txt.
func Pinned() (Checksums, error) { return ParseChecksums(pinnedFile) }

// ParseChecksums parses checksums.txt content; blank lines and lines starting
// with # are ignored.
func ParseChecksums(s string) (Checksums, error) {
	c := Checksums{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("bufdl: checksums line %d: want \"<version> <sha256> <asset>\"", n)
		}
		if b, err := hex.DecodeString(f[1]); err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("bufdl: checksums line %d: bad sha256 %q", n, f[1])
		}
		if c[f[0]] == nil {
			c[f[0]] = map[string]string{}
		}
		c[f[0]][f[2]] = strings.ToLower(f[1])
	}
	return c, sc.Err()
}

// Versions returns the pinned versions, oldest first.
func (c Checksums) Versions() []string {
	vs := make([]string, 0, len(c))
	for v := range c {
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool { return CompareVersions(vs[i], vs[j]) < 0 })
	return vs
}

// Format renders c in checksums.txt form, sorted by version and asset.
func (c Checksums) Format() string {
	var b strings.Builder
	for _, v := range c.Versions() {
		assets := make([]string, 0, len(c[v]))
		for a := range c[v] {
			assets = append(assets, a)
		}
		sort.Strings(assets)
		for _, a := range assets {
			fmt.Fprintf(&b, "%s %s %s\n", v, c[v][a], a)
		}
	}
	return b.String()
}

// CompareVersions compares dotted numeric versions ("1.32.2"); a leading "v"
// is ignored and missing components count as 0.
func CompareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "-")
	var out []int
	for p := range strings.SplitSeq(v, ".") {
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out = append(out, n)
	}
	return out
}

// Downloader fetches and verifies buf binaries.
type Downloader struct {
	// Dir caches binaries as <Dir>/<version>/buf.
	Dir string
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	// Client defaults to http.DefaultClient.
	Client *http.Client
	// Checksums defaults to Pinned().
	Checksums Checksums
	// GOOS/GOARCH default to the running platform.
	GOOS, GOARCH string
}

// ErrNotPinned means no checksum is pinned for the version and platform.
var ErrNotPinned = errors.New("bufdl: no pinned checksum")

// Get returns the path of a verified buf binary for version, downloading it
// if it is not cached (or the cached copy does not match its checksum).
func (d *Downloader) Get(ctx context.Context, version string) (string, error) {
	version = strings.TrimPrefix(version, "v")
	sums := d.Checksums
	if sums == nil {
		var err error
		if sums, err = Pinned(); err != nil {
			return "", err
		}
	}
	asset, err := Asset(orDefault(d.GOOS, runtime.GOOS), orDefault(d.GOARCH, runtime.GOARCH))
	if err != nil {
		return "", err
	}
	want := sums[version][asset]
	if want == "" {
		return "", fmt.Errorf("%w for buf %s (%s); run `go run ./test/conformance/cmd/fetchbuf pin %s`", ErrNotPinned, version, asset, version)
	}
	dest := filepath.Join(d.Dir, version, "buf")
	if got, err := fileSHA256(dest); err == nil && got == want {
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", err
	}
	url := strings.TrimSuffix(orDefault(d.BaseURL, DefaultBaseURL), "/") + "/v" + version + "/" + asset
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".buf-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	if err := d.fetch(ctx, url, io.MultiWriter(tmp, h)); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("bufdl: checksum mismatch for %s: got %s, pinned %s", url, got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // the binary must be executable
		return "", err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return "", err
	}
	return dest, nil
}

// FetchReleaseChecksums downloads sha256.txt of a release and returns the
// pinned assets' checksums. It trusts the release (trust on first use): the
// caller commits the result to checksums.txt for review.
func (d *Downloader) FetchReleaseChecksums(ctx context.Context, version string) (map[string]string, error) {
	version = strings.TrimPrefix(version, "v")
	url := strings.TrimSuffix(orDefault(d.BaseURL, DefaultBaseURL), "/") + "/v" + version + "/sha256.txt"
	var b strings.Builder
	if err := d.fetch(ctx, url, &b); err != nil {
		return nil, err
	}
	all := map[string]string{}
	for line := range strings.SplitSeq(b.String(), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			all[f[1]] = strings.ToLower(f[0])
		}
	}
	out := map[string]string{}
	for _, a := range PinnedAssets {
		if s, ok := all[a]; ok {
			out[a] = s
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("bufdl: %s lists none of %v", url, PinnedAssets)
	}
	return out, nil
}

func (d *Downloader) fetch(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c := d.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("bufdl: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bufdl: GET %s: %s", url, resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path is under the downloader's cache dir
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
