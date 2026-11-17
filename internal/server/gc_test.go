// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marijnkoesen/protodex/internal/blob"
	"github.com/marijnkoesen/protodex/internal/blob/fsblob"
	"github.com/marijnkoesen/protodex/internal/config"
	"github.com/marijnkoesen/protodex/internal/gc"
	"github.com/marijnkoesen/protodex/internal/server"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

// TestServeRunsGC checks the in-process wiring: Serve collects an old
// orphan on its interval and exposes the GC metrics.
func TestServeRunsGC(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	blobs, err := fsblob.New(filepath.Join(dir, "blobs"), 0)
	if err != nil {
		t.Fatal(err)
	}
	old, young := blob.DigestOf([]byte("old")), blob.DigestOf([]byte("young"))
	for d, c := range map[blob.Digest]string{old: "old", young: "young"} {
		if err := blobs.Put(ctx, d, strings.NewReader(c)); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "blobs", "sha256", old.Hex()[:2], old.Hex()), past, past); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(sctx, config.Default(), slog.New(slog.DiscardHandler), ln,
			server.WithStore(st), server.WithBlobs(blobs),
			server.WithGC(server.GCConfig{Interval: 5 * time.Millisecond, Grace: time.Hour}))
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()

	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if ok, _ := blobs.Exists(ctx, old); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old orphan not collected")
		}
	}
	if ok, _ := blobs.Exists(ctx, young); !ok {
		t.Fatal("orphan within the grace period collected")
	}
	resp, err := http.Get("http://" + ln.Addr().String() + "/metrics") //nolint:gosec,noctx // test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{`protodex_gc_runs_total{result="ok"}`, `protodex_gc_blobs_total{action="deleted"} 1`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

// TestConcurrentPushAndGC pushes content whose blobs already exist as old,
// unreferenced orphans while the collector sweeps continuously with no
// grace period. A push's blob write is then a no-op on an orphan the
// collector is entitled to delete; only the push's pins keep the collector
// from deleting it before the commit references it. Afterwards every
// committed manifest entry must still have its row and object.
func TestConcurrentPushAndGC(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	const (
		pool    = 8
		pushers = 4
		pushes  = 15
	)
	for p := range pushers {
		e.module(t, fmt.Sprintf("m%d", p))
	}
	content := func(i int) string { return fmt.Sprintf("syntax = \"proto3\";\n// shared %d\n", i) }
	seedOrphans := func() {
		for i := range pool {
			d := blob.DigestOf([]byte(content(i)))
			if err := e.blobs.Put(ctx, d, strings.NewReader(content(i))); err != nil {
				t.Error(err)
			}
		}
	}
	seedOrphans()

	col, err := gc.New(e.st, e.blobs, e.pins, gc.Options{Grace: -1, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	gcCtx, stopGC := context.WithCancel(ctx)
	var runs, deleted, spared atomic.Int64
	gcDone := make(chan struct{})
	go func() {
		defer close(gcDone)
		for gcCtx.Err() == nil {
			res, err := col.Collect(gcCtx)
			if err != nil && gcCtx.Err() == nil {
				t.Error(err)
			}
			runs.Add(1)
			deleted.Add(int64(res.Deleted))
			spared.Add(int64(res.InFlight + res.Rereferenced))
		}
	}()

	var wg sync.WaitGroup
	for p := range pushers {
		wg.Go(func() {
			for n := range pushes {
				// Two shared files (orphan candidates) plus one unique file
				// so every push makes a new commit.
				a, b := (p+n)%pool, (p+2*n+1)%pool
				files := protoFiles(
					"a.proto", content(a),
					"b.proto", content(b),
					"u.proto", fmt.Sprintf("syntax = \"proto3\";\n// %d-%d\n", p, n))
				if _, err := upload(e.up, fmt.Sprintf("m%d", p), files, nil); err != nil {
					t.Errorf("push %d/%d: %v", p, n, err)
					return
				}
				if n%5 == 0 {
					seedOrphans() // keep orphans around for the collector
				}
			}
		})
	}
	wg.Wait()
	// Let the collector finish at least one more full run after the pushes.
	for start := runs.Load(); runs.Load() < start+2; {
		time.Sleep(time.Millisecond)
	}
	stopGC()
	<-gcDone
	if _, err := col.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("gc runs %d, deleted %d, spared %d", runs.Load(), deleted.Load(), spared.Load())

	commits := 0
	mods, err := e.st.ListAllModules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mods {
		cs, err := e.st.ListCommits(ctx, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			commits++
			man, err := e.st.GetManifest(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, me := range man {
				if _, err := e.st.GetBlob(ctx, me.BlobDigest); err != nil {
					t.Fatalf("commit %s %s: blob row: %v", c.ID, me.Path, err)
				}
				d, _ := blob.ParseDigest(me.BlobDigest)
				r, err := e.blobs.Get(ctx, d)
				if err != nil {
					t.Fatalf("commit %s %s: blob object: %v", c.ID, me.Path, err)
				}
				b, _ := io.ReadAll(r)
				_ = r.Close()
				if blob.DigestOf(b) != d {
					t.Fatalf("commit %s %s: corrupt blob", c.ID, me.Path)
				}
			}
		}
	}
	if commits != pushers*pushes {
		t.Fatalf("%d commits, want %d", commits, pushers*pushes)
	}
}
