// SPDX-License-Identifier: Apache-2.0

package b5_test

import (
	"strings"
	"testing"

	"github.com/marijnkoesen/protodex/internal/b5"
)

// The expected digests were produced by bufbuild/buf v1.57.0's own bufcas
// code (manifest digest + sorted dependency digests).
func TestDigestMatchesBufVectors(t *testing.T) {
	const base = "b5:776d1c1c7264cf08666068f902468798f7dfd0978bb43e06ad0a7925042e82353626602ed540c44fc68b649697908189b810c33d5081b54b04ef21a29e0a0b29"
	tests := []struct {
		name  string
		files []b5.File
		deps  []string
		want  string
	}{
		{"single", []b5.File{{Path: "b.proto", Content: []byte("// b")}}, nil, base},
		{"two files", []b5.File{{Path: "a/v1/a.proto", Content: []byte(`syntax = "proto3";`)}, {Path: "LICENSE", Content: []byte("Apache")}}, nil,
			"b5:9d4dd70e0746b7050cdf2222f2b66d3e0cd03543d0ca40d1a7fdd28ce41a0a897351300bb11d6ab6631442ff477f89000ee2a25f98e24147b55496d4bc6b4d15"},
		{"dep", []b5.File{{Path: "a.proto", Content: []byte("// a")}}, []string{base},
			"b5:ee0e33a0f570ed52ecf1f681c5c78a5ba2cdcad7dab40bf93ad52f2dcc258a0be9552939a5cc08eb25d8d053d0d9997c6e9ea75a032a3bb4616a2d2caa686606"},
		{"empty file", []b5.File{{Path: "a.proto"}}, nil,
			"b5:4350328b840ad57d2f3c96f44445a77b09a7ca20c8897dfb4cc7c891786d1e18d8e88f43361a2820c45315ed36492e4a299c369f22b103eb64e90bf6921b12c8"},
		{"path ordering", []b5.File{{Path: "a.b.proto", Content: []byte("4")}, {Path: "a/b.proto", Content: []byte("2")},
			{Path: "a.proto", Content: []byte("1")}, {Path: "a-b.proto", Content: []byte("3")}}, nil,
			"b5:735248038ae3a5c1102bd7847c99dde48742bee38c8823a94823074deff38189fe26e23bee09bd052f2ad2f7e08bce74112214f7161f3a07cbb7d7e568912837"},
	}
	for _, tc := range tests {
		got, err := b5.Digest(tc.files, tc.deps)
		if err != nil || got != tc.want {
			t.Errorf("%s: Digest = %s, %v; want %s", tc.name, got, err, tc.want)
		}
	}
}

func TestDigestRejectsBadInput(t *testing.T) {
	f := []b5.File{{Path: "a.proto"}}
	for _, p := range []string{"", "/a.proto", "../a.proto", "a//b.proto", "./a.proto", `a\b.proto`} {
		if _, err := b5.Digest([]b5.File{{Path: p}}, nil); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	if _, err := b5.Digest(append(f, f...), nil); err == nil {
		t.Error("duplicate path accepted")
	}
	if _, err := b5.Digest(f, []string{"b4:abc"}); err == nil {
		t.Error("non-b5 dependency accepted")
	}
}

func TestFilter(t *testing.T) {
	in := []b5.File{{Path: "z.proto"}, {Path: "buf.yaml"}, {Path: "README.md"}, {Path: "buf.md"}, {Path: "LICENSE"}, {Path: "a.txt"}, {Path: "x/LICENSE"}, {Path: "a.proto"}}
	var got []string
	for _, f := range b5.Filter(in) {
		got = append(got, f.Path)
	}
	if want := "LICENSE a.proto buf.md z.proto"; strings.Join(got, " ") != want {
		t.Fatalf("Filter = %v, want %s", got, want)
	}
}

func TestBytes(t *testing.T) {
	if b, err := b5.Bytes("b5:00ff"); err != nil || len(b) != 2 || b[1] != 0xff {
		t.Fatalf("Bytes = %v, %v", b, err)
	}
	for _, s := range []string{"", "b5:", "b5:zz", "shake256:00"} {
		if _, err := b5.Bytes(s); err == nil {
			t.Errorf("Bytes(%q) accepted", s)
		}
	}
}
