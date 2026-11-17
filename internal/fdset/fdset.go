// SPDX-License-Identifier: Apache-2.0

// Package fdset compiles a module's .proto files (plus those of its
// dependencies) into a FileDescriptorSet, and caches the results by commit
// digest.
package fdset

import (
	"container/list"
	"context"
	"fmt"
	"sync"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/options"
	"github.com/bufbuild/protocompile/protoutil"
	"github.com/bufbuild/protocompile/reporter"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Options select what Build puts in the set.
type Options struct {
	// ExcludeImports leaves out every file that is not a target: files of
	// dependencies and well-known types the module does not contain.
	ExcludeImports bool
	// SourceCodeInfo keeps source code info (locations and comments).
	SourceCodeInfo bool
	// SourceRetentionOptions keeps options with source retention.
	SourceRetentionOptions bool
}

// CompileError is a module that does not compile.
type CompileError struct{ Err error }

func (e *CompileError) Error() string { return "compiling module: " + e.Err.Error() }

func (e *CompileError) Unwrap() error { return e.Err }

// Build compiles the target files. sources holds every file that may be
// imported (targets included), by path; the well-known types are always
// available. The result holds the targets and, unless excluded, everything
// they import, in topological order (imports before importers).
func Build(ctx context.Context, sources map[string]string, targets []string, opts Options) (*descriptorpb.FileDescriptorSet, error) {
	mode := protocompile.SourceInfoNone
	if opts.SourceCodeInfo {
		mode = protocompile.SourceInfoStandard
	}
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: protocompile.SourceAccessorFromMap(sources),
		}),
		SourceInfoMode: mode,
		Reporter:       reporter.NewReporter(nil, nil), // fail on the first error, ignore warnings
	}
	files, err := c.Compile(ctx, targets...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &CompileError{Err: err}
	}

	isTarget := make(map[string]bool, len(targets))
	for _, t := range targets {
		isTarget[t] = true
	}
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var add func(fd protoreflect.FileDescriptor) error
	add = func(fd protoreflect.FileDescriptor) error {
		if seen[fd.Path()] {
			return nil
		}
		seen[fd.Path()] = true
		imports := fd.Imports()
		for i := range imports.Len() {
			if err := add(imports.Get(i).FileDescriptor); err != nil {
				return err
			}
		}
		if opts.ExcludeImports && !isTarget[fd.Path()] {
			return nil
		}
		fdp := protoutil.ProtoFromFileDescriptor(fd)
		if !opts.SourceRetentionOptions {
			stripped, err := options.StripSourceRetentionOptionsFromFile(fdp)
			if err != nil {
				return fmt.Errorf("stripping source-retention options of %s: %w", fd.Path(), err)
			}
			fdp = stripped
		}
		if !opts.SourceCodeInfo && fdp.SourceCodeInfo != nil {
			fdp = proto.CloneOf(fdp)
			fdp.SourceCodeInfo = nil
		}
		set.File = append(set.File, fdp)
		return nil
	}
	for _, f := range files {
		if err := add(f); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// Key identifies a cached set: the commit digest (which covers the files
// and the dependency digests, so it pins all inputs) and the options.
type Key struct {
	Digest string
	Opts   Options
}

// Cache is a size-bounded LRU of built sets. Cached sets are shared and
// must not be modified. It is safe for concurrent use.
type Cache struct {
	mu       sync.Mutex
	maxBytes int
	bytes    int
	order    *list.List // front = most recently used
	entries  map[Key]*list.Element
}

type cacheEntry struct {
	key  Key
	set  *descriptorpb.FileDescriptorSet
	size int
}

// NewCache returns a cache holding at most maxBytes of (serialized) sets.
// A set larger than maxBytes is never cached.
func NewCache(maxBytes int) *Cache {
	return &Cache{maxBytes: maxBytes, order: list.New(), entries: map[Key]*list.Element{}}
}

// Get returns the cached set for k.
func (c *Cache) Get(k Key) (*descriptorpb.FileDescriptorSet, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*cacheEntry).set, true
}

// Put stores set under k, evicting the least recently used sets as needed.
func (c *Cache) Put(k Key, set *descriptorpb.FileDescriptorSet) {
	if c == nil {
		return
	}
	size := proto.Size(set)
	if size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[k]; ok {
		c.order.MoveToFront(e)
		return
	}
	c.entries[k] = c.order.PushFront(&cacheEntry{key: k, set: set, size: size})
	c.bytes += size
	for c.bytes > c.maxBytes {
		last := c.order.Back()
		ce := last.Value.(*cacheEntry)
		c.order.Remove(last)
		delete(c.entries, ce.key)
		c.bytes -= ce.size
	}
}

// Len returns the number of cached sets.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
