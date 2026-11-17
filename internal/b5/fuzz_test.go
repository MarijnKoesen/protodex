// SPDX-License-Identifier: Apache-2.0

package b5_test

import (
	"testing"

	"github.com/marijnkoesen/protodex/internal/b5"
)

// FuzzBytes checks that decoding arbitrary digest strings never panics and
// that accepted digests are well formed.
func FuzzBytes(f *testing.F) {
	f.Add("b5:00ff")
	f.Add("b5:")
	f.Add("shake256:ab")
	f.Add("b5:zz")
	f.Fuzz(func(t *testing.T, s string) {
		b, err := b5.Bytes(s)
		if err == nil && len(b) == 0 {
			t.Fatalf("Bytes(%q) accepted an empty digest", s)
		}
	})
}

// FuzzDigest checks that digesting arbitrary paths, contents and dependency
// digests never panics, is deterministic and independent of file order.
func FuzzDigest(f *testing.F) {
	f.Add("a.proto", "// a", "b.proto", "// b", "b5:00")
	f.Add("../x", "", "a/../b", "", "")
	f.Add("a.proto", "x", "a.proto", "y", "b5:ff")
	f.Fuzz(func(t *testing.T, p1, c1, p2, c2, dep string) {
		files := []b5.File{{Path: p1, Content: []byte(c1)}, {Path: p2, Content: []byte(c2)}}
		var deps []string
		if dep != "" {
			deps = []string{dep}
		}
		d1, err1 := b5.Digest(files, deps)
		d2, err2 := b5.Digest([]b5.File{files[1], files[0]}, deps)
		if (err1 == nil) != (err2 == nil) || d1 != d2 {
			t.Fatalf("order-dependent result: %q/%v vs %q/%v", d1, err1, d2, err2)
		}
		if err1 == nil {
			if _, err := b5.Bytes(d1); err != nil {
				t.Fatalf("Digest produced undecodable %q: %v", d1, err)
			}
		}
		_ = b5.Filter(files)
	})
}
