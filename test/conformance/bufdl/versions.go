// SPDX-License-Identifier: Apache-2.0

package bufdl

import (
	_ "embed" // versions.txt
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed versions.txt
var versionsFile string

// Supported returns the buf CLI versions of the conformance matrix, oldest
// first, from versions.txt. With prOnly it returns only the versions marked
// "pr" (the subset run on every pull request).
func Supported(prOnly bool) ([]string, error) {
	pinned, err := Pinned()
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(versionsFile, "\n") {
		line, _, _ = strings.Cut(line, "#")
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if _, ok := pinned[f[0]]; !ok {
			return nil, fmt.Errorf("bufdl: version %s in versions.txt has no pinned checksums", f[0])
		}
		if prOnly && (len(f) < 2 || f[1] != "pr") {
			continue
		}
		out = append(out, f[0])
	}
	return out, nil
}

// DefaultCacheDir is where downloaded binaries are kept by default.
func DefaultCacheDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "protodex", "buf")
	}
	return filepath.Join(os.TempDir(), "protodex-buf")
}
