// SPDX-License-Identifier: Apache-2.0

// Package blobtest holds the contract test suite every blob.Store must pass.
package blobtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marijnkoesen/protodex/internal/blob"
)

// Factory returns a fresh, empty store limited to maxSize bytes.
type Factory func(t *testing.T, maxSize int64) blob.Store

// Run executes the contract suite against stores from f.
func Run(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("roundtrip", func(t *testing.T) {
		s := f(t, 0)
		data := []byte("hello protodex")
		d := blob.DigestOf(data)
		if ok, err := s.Exists(ctx, d); err != nil || ok {
			t.Fatalf("Exists before put = %v, %v", ok, err)
		}
		if err := s.Put(ctx, d, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.Exists(ctx, d); err != nil || !ok {
			t.Fatalf("Exists after put = %v, %v", ok, err)
		}
		if got := mustGet(t, s, d); !bytes.Equal(got, data) {
			t.Fatalf("got %q", got)
		}
		// Idempotent.
		if err := s.Put(ctx, d, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("empty blob", func(t *testing.T) {
		s := f(t, 0)
		d := blob.DigestOf(nil)
		if err := s.Put(ctx, d, bytes.NewReader(nil)); err != nil {
			t.Fatal(err)
		}
		if got := mustGet(t, s, d); len(got) != 0 {
			t.Fatalf("got %d bytes", len(got))
		}
	})

	t.Run("list", func(t *testing.T) {
		s := f(t, 0)
		l, ok := s.(blob.Lister)
		if !ok {
			t.Skip("store does not implement blob.Lister")
		}
		want := map[blob.Digest]int64{}
		for _, data := range []string{"one", "two!", ""} {
			d := blob.DigestOf([]byte(data))
			if err := s.Put(ctx, d, bytes.NewReader([]byte(data))); err != nil {
				t.Fatal(err)
			}
			want[d] = int64(len(data))
		}
		gone := blob.DigestOf([]byte("two!"))
		if err := s.Delete(ctx, gone); err != nil {
			t.Fatal(err)
		}
		delete(want, gone)
		got := map[blob.Digest]int64{}
		before := time.Now().Add(-time.Hour)
		err := l.List(ctx, func(i blob.Info) error {
			if i.ModTime.Before(before) {
				t.Errorf("%s: ModTime %v is too old", i.Digest, i.ModTime)
			}
			got[i.Digest] = i.Size
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("List = %v, want %v", got, want)
		}
		for d, n := range want {
			if got[d] != n {
				t.Fatalf("List = %v, want %v", got, want)
			}
		}
		stop := errors.New("stop")
		if err := l.List(ctx, func(blob.Info) error { return stop }); !errors.Is(err, stop) {
			t.Fatalf("List error = %v, want the callback's error", err)
		}
	})

	t.Run("get missing", func(t *testing.T) {
		s := f(t, 0)
		if _, err := s.Get(ctx, blob.DigestOf([]byte("nope"))); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		s := f(t, 0)
		data := []byte("to delete")
		d := blob.DigestOf(data)
		if err := s.Delete(ctx, d); err != nil {
			t.Fatalf("delete missing: %v", err)
		}
		if err := s.Put(ctx, d, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, d); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.Exists(ctx, d); ok {
			t.Fatal("still exists")
		}
	})

	t.Run("digest mismatch stores nothing", func(t *testing.T) {
		s := f(t, 0)
		d := blob.DigestOf([]byte("expected"))
		err := s.Put(ctx, d, bytes.NewReader([]byte("actual")))
		if !errors.Is(err, blob.ErrDigestMismatch) {
			t.Fatalf("err = %v, want ErrDigestMismatch", err)
		}
		if ok, _ := s.Exists(ctx, d); ok {
			t.Fatal("mismatched blob became visible")
		}
	})

	t.Run("size limit", func(t *testing.T) {
		s := f(t, 1024)
		data := bytes.Repeat([]byte("x"), 2048)
		d := blob.DigestOf(data)
		if err := s.Put(ctx, d, bytes.NewReader(data)); !errors.Is(err, blob.ErrTooLarge) {
			t.Fatalf("err = %v, want ErrTooLarge", err)
		}
		if ok, _ := s.Exists(ctx, d); ok {
			t.Fatal("oversized blob became visible")
		}
		ok := bytes.Repeat([]byte("y"), 1024)
		if err := s.Put(ctx, blob.DigestOf(ok), bytes.NewReader(ok)); err != nil {
			t.Fatalf("blob at limit: %v", err)
		}
	})

	t.Run("invalid digest", func(t *testing.T) {
		s := f(t, 0)
		if _, err := s.Get(ctx, blob.Digest{}); !errors.Is(err, blob.ErrInvalidDigest) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("large blob bounded memory", func(t *testing.T) {
		if testing.Short() {
			t.Skip("short mode")
		}
		const size = 64 << 20
		const bound = 48 << 20
		s := f(t, 0)

		// Compute the digest by streaming a deterministic pattern.
		h := sha256.New()
		if _, err := io.Copy(h, &patternReader{n: size}); err != nil {
			t.Fatal(err)
		}
		d, err := blob.ParseDigest("sha256:" + hex.EncodeToString(h.Sum(nil)))
		if err != nil {
			t.Fatal(err)
		}

		runtime.GC()
		var base runtime.MemStats
		runtime.ReadMemStats(&base)
		var peak atomic.Uint64
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			var m runtime.MemStats
			for {
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak.Load() {
					peak.Store(m.HeapAlloc)
				}
				select {
				case <-stop:
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}()
		err = s.Put(ctx, d, &patternReader{n: size})
		close(stop)
		<-done
		if err != nil {
			t.Fatal(err)
		}
		if used := int64(peak.Load() - base.HeapAlloc); used > bound { //nolint:gosec // bounded by test sizes
			t.Fatalf("heap grew by %d MiB uploading %d MiB (bound %d MiB)", used>>20, size>>20, bound>>20)
		}

		// Streaming read back, verified by hash.
		rc, err := s.Get(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rc.Close() }()
		h.Reset()
		n, err := io.Copy(h, rc)
		if err != nil || n != size || hex.EncodeToString(h.Sum(nil)) != d.Hex() {
			t.Fatalf("readback n=%d err=%v", n, err)
		}
	})
}

func mustGet(t *testing.T, s blob.Store, d blob.Digest) []byte {
	t.Helper()
	rc, err := s.Get(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// patternReader yields n bytes of a repeating, non-trivial pattern.
type patternReader struct{ n, off int64 }

func (r *patternReader) Read(p []byte) (int, error) {
	if r.off >= r.n {
		return 0, io.EOF
	}
	if rem := r.n - r.off; int64(len(p)) > rem {
		p = p[:rem]
	}
	// The pattern repeats every 2048 bytes: (x*31>>3)&0xff depends on x mod 2048.
	for i := 0; i < len(p); {
		i += copy(p[i:], patternBlock[(r.off+int64(i))%int64(len(patternBlock)):])
	}
	r.off += int64(len(p))
	return len(p), nil
}

var patternBlock = func() []byte {
	b := make([]byte, 2048)
	for i := range b {
		b[i] = byte((int64(i) * 31 >> 3) & 0xff)
	}
	return b
}()
