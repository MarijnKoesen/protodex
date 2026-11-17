// SPDX-License-Identifier: Apache-2.0

package gc_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marijnkoesen/protodex/internal/blob"
	"github.com/marijnkoesen/protodex/internal/blob/fsblob"
	"github.com/marijnkoesen/protodex/internal/gc"
	"github.com/marijnkoesen/protodex/internal/observability"
	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

type env struct {
	st     *sqlite.Store
	blobs  *fsblob.Store
	pins   *gc.Pins
	commit store.Commit
}

func newEnv(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	blobs, err := fsblob.New(filepath.Join(t.TempDir(), "blobs"), 0)
	if err != nil {
		t.Fatal(err)
	}
	o, err := st.CreateOwner(ctx, store.Owner{Name: "acme", Type: store.OwnerOrganization})
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.CreateModule(ctx, store.Module{OwnerID: o.ID, Name: "m", Visibility: store.VisibilityPublic, DefaultLabel: "main"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.CreateCommit(ctx, store.Commit{ModuleID: m.ID, Digest: "b5:00"})
	if err != nil {
		t.Fatal(err)
	}
	return env{st: st, blobs: blobs, pins: gc.NewPins(), commit: c}
}

// put stores content in the blob store and, if row, as a blob row.
func (e env) put(t *testing.T, content string, row bool) string {
	t.Helper()
	ctx := context.Background()
	d := blob.DigestOf([]byte(content))
	if err := e.blobs.Put(ctx, d, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if row {
		if err := e.st.PutBlob(ctx, store.Blob{Digest: d.String(), Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
	}
	return d.String()
}

func (e env) reference(t *testing.T, digests ...string) {
	t.Helper()
	var es []store.ManifestEntry
	for i, d := range digests {
		es = append(es, store.ManifestEntry{Path: string(rune('a'+i)) + ".proto", BlobDigest: d})
	}
	if err := e.st.SetManifest(context.Background(), e.commit.ID, es); err != nil {
		t.Fatal(err)
	}
}

// present reports whether d exists as an object and as a row.
func (e env) present(t *testing.T, d string) (object, row bool) {
	t.Helper()
	ctx := context.Background()
	pd, err := blob.ParseDigest(d)
	if err != nil {
		t.Fatal(err)
	}
	object, err = e.blobs.Exists(ctx, pd)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.st.GetBlob(ctx, d)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return object, err == nil
}

func (e env) collector(t *testing.T, opts gc.Options) *gc.Collector {
	t.Helper()
	c, err := gc.New(e.st, e.blobs, e.pins, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// later makes every existing blob older than the default grace period.
func later() time.Time { return time.Now().Add(gc.DefaultGrace + time.Hour) }

func TestCollect(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	kept := e.put(t, "referenced", true)
	e.reference(t, kept)
	orphanRow := e.put(t, "orphan with row", true)     // e.g. module deleted
	orphanObj := e.put(t, "orphan object only", false) // e.g. failed push
	rowOnly := "sha256:" + strings.Repeat("ab", 32)
	if err := e.st.PutBlob(ctx, store.Blob{Digest: rowOnly, Data: []byte("inline")}); err != nil {
		t.Fatal(err)
	}

	m := observability.NewMetrics()
	// Within the grace period only the row without an object goes.
	res, err := e.collector(t, gc.Options{Metrics: m}).Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 3 || res.Rows != 3 || res.Referenced != 1 || res.Young != 2 || res.Candidates != 1 || res.Deleted != 1 {
		t.Fatalf("within grace: %+v", res)
	}
	if _, row := e.present(t, orphanRow); !row {
		t.Fatal("young orphan deleted")
	}

	// Dry run past the grace period: reports, deletes nothing.
	res, err = e.collector(t, gc.Options{DryRun: true, Now: later, Metrics: m}).Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := int64(len("orphan with row") + len("orphan object only"))
	if !res.DryRun || res.Candidates != 2 || res.Deleted != 2 || res.DeletedBytes != wantBytes {
		t.Fatalf("dry run: %+v", res)
	}
	for _, d := range []string{orphanRow, orphanObj} {
		if obj, _ := e.present(t, d); !obj {
			t.Fatalf("dry run deleted %s", d)
		}
	}

	// For real.
	res, err = e.collector(t, gc.Options{Now: later, Metrics: m}).Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 || res.DeletedBytes != wantBytes || res.Failed != 0 {
		t.Fatalf("collect: %+v", res)
	}
	for _, d := range []string{orphanRow, orphanObj} {
		if obj, row := e.present(t, d); obj || row {
			t.Fatalf("%s still present: object %v row %v", d, obj, row)
		}
	}
	if obj, row := e.present(t, kept); !obj || !row {
		t.Fatal("referenced blob deleted")
	}

	// Nothing left to do.
	if res, err = e.collector(t, gc.Options{Now: later}).Collect(ctx); err != nil || res.Candidates != 0 {
		t.Fatalf("second run: %+v %v", res, err)
	}

	for name, want := range map[string]float64{
		`protodex_gc_runs_total{result="ok"}`:       3,
		`protodex_gc_blobs_total{action="deleted"}`: 3,
		`protodex_gc_blobs_total{action="dry_run"}`: 2,
		`protodex_gc_bytes_total{action="deleted"}`: float64(wantBytes), // rows without an object have no stored size
	} {
		if got := metricValue(t, m, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

func TestCollectSparesPinnedAndRereferenced(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	pinned := e.put(t, "pinned", false)
	release, err := e.pins.Pin(ctx, []string{pinned})
	if err != nil {
		t.Fatal(err)
	}
	c := e.collector(t, gc.Options{Now: later})
	res, err := c.Collect(ctx)
	if err != nil || res.InFlight != 1 || res.Deleted != 0 {
		t.Fatalf("pinned: %+v %v", res, err)
	}
	if obj, _ := e.present(t, pinned); !obj {
		t.Fatal("pinned blob deleted")
	}
	release()
	release() // idempotent
	if e.pins.Pinned(pinned) != 0 {
		t.Fatal("pin not released")
	}
	if res, err = c.Collect(ctx); err != nil || res.Deleted != 1 {
		t.Fatalf("after release: %+v %v", res, err)
	}
}

func TestPinWaitsForClaim(t *testing.T) {
	p := gc.NewPins()
	release, ok := p.Claim("d")
	if !ok {
		t.Fatal("claim failed")
	}
	if _, ok := p.Claim("d"); ok {
		t.Fatal("double claim succeeded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Pin(ctx, []string{"x", "d"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Pin while claimed = %v", err)
	}
	if p.Pinned("x") != 0 {
		t.Fatal("failed Pin left a pin behind")
	}
	pinned := make(chan func())
	go func() {
		r, err := p.Pin(context.Background(), []string{"d"})
		if err != nil {
			t.Error(err)
		}
		pinned <- r
	}()
	select {
	case <-pinned:
		t.Fatal("Pin did not wait for the claim")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	unpin := <-pinned
	if _, ok := p.Claim("d"); ok {
		t.Fatal("claimed a pinned digest")
	}
	unpin()
	if r, ok := p.Claim("d"); !ok {
		t.Fatal("claim after unpin failed")
	} else {
		r()
	}
}

func TestNewRequiresLister(t *testing.T) {
	e := newEnv(t)
	if _, err := gc.New(e.st, struct{ blob.Store }{e.blobs}, e.pins, gc.Options{}); err == nil {
		t.Fatal("expected error for a store without List")
	}
}

type leader struct{ calls chan struct{} }

func (l leader) Lead(context.Context) (func(), bool, error) {
	l.calls <- struct{}{}
	return func() {}, false, nil // never leads
}

func TestRun(t *testing.T) {
	e := newEnv(t)
	orphan := e.put(t, "orphan", false)
	c := e.collector(t, gc.Options{Grace: -1})

	// A non-leader never collects.
	ctx, cancel := context.WithCancel(context.Background())
	l := leader{calls: make(chan struct{}, 10)}
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, time.Millisecond, l) }()
	<-l.calls
	<-l.calls
	cancel()
	<-done
	if obj, _ := e.present(t, orphan); !obj {
		t.Fatal("non-leader collected")
	}

	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { defer close(done); c.Run(ctx, time.Millisecond, nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if obj, _ := e.present(t, orphan); !obj {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not collect")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func metricValue(t *testing.T, m *observability.Metrics, series string) float64 {
	t.Helper()
	name, labels, _ := strings.Cut(series, "{")
	labels = strings.TrimSuffix(labels, "}")
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, mt := range mf.GetMetric() {
			var parts []string
			for _, lp := range mt.GetLabel() {
				parts = append(parts, lp.GetName()+`="`+lp.GetValue()+`"`)
			}
			if strings.Join(parts, ",") == labels {
				return mt.GetCounter().GetValue()
			}
		}
	}
	return 0
}
