// SPDX-License-Identifier: Apache-2.0

// Package b5 computes the module digests the buf CLI expects.
//
// bufbuild/buf's content-addressing code lives in private packages that panic
// at start-up when imported by other projects, so it is reimplemented here
// from the Apache-2.0 client source (bufcas and bufmodule): a file digest is
// SHAKE256 (64 bytes) of the file; a module's files digest is the SHAKE256
// of its manifest (one "shake256:<hex>  <path>" line per file, sorted by
// path); the module digest, "b5:<hex>", is the SHAKE256 of the files digest
// and the sorted dependency digests joined by newlines. The module and
// dependency names have no effect on the digest.
package b5

import (
	"crypto/sha3"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// Prefix is the algorithm prefix of a b5 digest string ("b5:<hex>").
const Prefix = "b5:"

// File is a module file.
type File struct {
	Path    string
	Content []byte
}

const licensePath = "LICENSE"

// docPaths are the candidate documentation files; only the first one present
// is part of a module.
var docPaths = []string{"buf.md", "README.md", "README.markdown"}

// Filter returns the files that belong to a module: *.proto files, LICENSE
// and the documentation file. Everything else (buf.yaml, stray files) is
// not part of a module and does not affect its digest. The result is sorted
// by path.
func Filter(files []File) []File {
	doc := ""
	for _, p := range docPaths {
		if slices.ContainsFunc(files, func(f File) bool { return f.Path == p }) {
			doc = p
			break
		}
	}
	var out []File
	for _, f := range files {
		if path.Ext(f.Path) == ".proto" || f.Path == licensePath || (doc != "" && f.Path == doc) {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return out
}

const shakeLen = 64

func shake(data string) string { return hex.EncodeToString(sha3.SumSHAKE256([]byte(data), shakeLen)) }

// validPath reports whether p is a normalized, relative, slash-separated path.
func validPath(p string) bool {
	return p != "" && p == path.Clean(p) && !path.IsAbs(p) && p != ".." && !strings.HasPrefix(p, "../") && !strings.Contains(p, `\`)
}

// Digest returns the "b5:<hex>" digest of the module made of files (already
// filtered) and the b5 digests of its dependencies. File and dependency
// order do not matter.
func Digest(files []File, depDigests []string) (string, error) {
	lines := make([]string, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		if !validPath(f.Path) {
			return "", fmt.Errorf("invalid file path %q", f.Path)
		}
		if seen[f.Path] {
			return "", fmt.Errorf("duplicate file path %q", f.Path)
		}
		seen[f.Path] = true
		lines = append(lines, f.Path+"\x00shake256:"+hex.EncodeToString(sha3.SumSHAKE256(f.Content, shakeLen)))
	}
	slices.Sort(lines) // by path: the NUL separator sorts before any path byte
	var manifest strings.Builder
	for _, l := range lines {
		p, d, _ := strings.Cut(l, "\x00")
		manifest.WriteString(d + "  " + p + "\n")
	}
	all := []string{"shake256:" + shake(manifest.String())}
	for _, d := range depDigests {
		if !strings.HasPrefix(d, Prefix) {
			return "", fmt.Errorf("dependency digest %q is not a b5 digest", d)
		}
	}
	all = append(all, slices.Sorted(slices.Values(depDigests))...)
	return Prefix + shake(strings.Join(all, "\n")), nil
}

// ErrInvalid means a digest string is not a well-formed b5 digest.
var ErrInvalid = errors.New("invalid b5 digest")

// Bytes decodes a "b5:<hex>" digest into its raw value.
func Bytes(s string) ([]byte, error) {
	h, ok := strings.CutPrefix(s, Prefix)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	return b, nil
}
