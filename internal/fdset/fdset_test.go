// SPDX-License-Identifier: Apache-2.0

package fdset_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/marijnkoesen/protodex/internal/fdset"
)

const optsProto = `syntax = "proto3";
package opts;
import "google/protobuf/descriptor.proto";
extend google.protobuf.MessageOptions {
  string note = 50001 [retention = RETENTION_SOURCE];
  string tag = 50002;
}
`

const userProto = `syntax = "proto3";
package user;
import "opts/opts.proto";
// A user.
message User {
  option (opts.note) = "source only";
  option (opts.tag) = "kept";
  string name = 1;
}
`

func names(set *descriptorpb.FileDescriptorSet) (out []string) {
	for _, f := range set.GetFile() {
		out = append(out, f.GetName())
	}
	return out
}

func TestBuild(t *testing.T) {
	ctx := context.Background()
	srcs := map[string]string{"opts/opts.proto": optsProto, "user/user.proto": userProto}

	set, err := fdset.Build(ctx, srcs, []string{"user/user.proto"}, fdset.Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"google/protobuf/descriptor.proto", "opts/opts.proto", "user/user.proto"}
	if got := names(set); !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	user := set.GetFile()[2]
	if user.GetSourceCodeInfo() != nil {
		t.Fatal("source info present")
	}
	// The source-retention option is stripped, the runtime one kept (both
	// are unknown fields here, as the extensions are not linked in).
	raw, _ := proto.Marshal(user.GetMessageType()[0].GetOptions())
	stripped := len(raw)

	set, err = fdset.Build(ctx, srcs, []string{"user/user.proto"}, fdset.Options{ExcludeImports: true, SourceCodeInfo: true, SourceRetentionOptions: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(set); !slices.Equal(got, []string{"user/user.proto"}) {
		t.Fatalf("exclude imports: files = %v", got)
	}
	user = set.GetFile()[0]
	if user.GetSourceCodeInfo() == nil {
		t.Fatal("no source info")
	}
	raw, _ = proto.Marshal(user.GetMessageType()[0].GetOptions())
	if stripped == 0 || len(raw) <= stripped {
		t.Fatalf("options size: stripped %d, with source retention %d", stripped, len(raw))
	}

	_, err = fdset.Build(ctx, map[string]string{"x.proto": `syntax = "proto3"; message X { Y y = 1; }`}, []string{"x.proto"}, fdset.Options{})
	var ce *fdset.CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want CompileError", err)
	}
}

func TestCache(t *testing.T) {
	set := func(n int) *descriptorpb.FileDescriptorSet {
		s := &descriptorpb.FileDescriptorSet{}
		for range n {
			s.File = append(s.File, &descriptorpb.FileDescriptorProto{Name: proto.String("0123456789.proto")})
		}
		return s
	}
	one := proto.Size(set(1))
	c := fdset.NewCache(3 * one)
	k := func(d string) fdset.Key { return fdset.Key{Digest: d} }

	c.Put(k("a"), set(1))
	c.Put(k("b"), set(1))
	c.Put(k("c"), set(1))
	if _, ok := c.Get(k("a")); !ok { // a is now most recently used
		t.Fatal("a missing")
	}
	c.Put(k("d"), set(1)) // evicts b
	if _, ok := c.Get(k("b")); ok {
		t.Fatal("b not evicted")
	}
	for _, d := range []string{"a", "c", "d"} {
		if _, ok := c.Get(k(d)); !ok {
			t.Fatalf("%s evicted", d)
		}
	}
	// Options are part of the key.
	if _, ok := c.Get(fdset.Key{Digest: "a", Opts: fdset.Options{ExcludeImports: true}}); ok {
		t.Fatal("options ignored")
	}
	// Too large to cache.
	c.Put(k("big"), set(4))
	if _, ok := c.Get(k("big")); ok || c.Len() != 3 {
		t.Fatalf("oversized set cached; len %d", c.Len())
	}
	var nilCache *fdset.Cache
	nilCache.Put(k("a"), set(1))
	if _, ok := nilCache.Get(k("a")); ok {
		t.Fatal("nil cache hit")
	}
}
