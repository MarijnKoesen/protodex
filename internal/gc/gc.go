// SPDX-License-Identifier: Apache-2.0

// Package gc garbage-collects blobs that no commit references any more:
// file contents left behind by deleted modules and by pushes that stored
// their blobs but then failed before committing.
//
// A run is a mark-and-sweep. Mark: list the blob store and the blob rows,
// and read the set of digests some manifest references. Sweep: every
// unreferenced blob older than the grace period is claimed (see Leases),
// re-checked against the database and only then deleted, row first, then
// the stored object. The re-check under a claim is what makes the sweep
// safe against concurrent pushes; the grace period is defence in depth (and
// keeps freshly written blobs around for debugging).
//
// In SQLite mode the collector runs in-process on a timer (Run). For a
// multi-replica Postgres deployment, Run takes a Leader so that exactly one
// replica collects, and Leases needs a shared implementation.
package gc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/marijnkoesen/protodex/internal/blob"
	"github.com/marijnkoesen/protodex/internal/observability"
	"github.com/marijnkoesen/protodex/internal/store"
)

// DefaultGrace is the default minimum age of a blob before it is collected.
const DefaultGrace = 24 * time.Hour

// Options configure a Collector.
type Options struct {
	// Grace is the minimum age of a stored blob before it may be deleted.
	// Zero selects DefaultGrace; a negative value disables the grace period
	// (tests only).
	Grace time.Duration
	// DryRun reports what would be deleted without deleting anything.
	DryRun bool
	// Metrics, when set, receives run and blob counters.
	Metrics *observability.Metrics
	// Logger receives a summary per run; nil discards.
	Logger *slog.Logger
	// Now returns the current time; nil selects time.Now.
	Now func() time.Time
}

// Collector deletes unreferenced blobs. Collect and Run are safe for
// concurrent use; runs never overlap.
type Collector struct {
	st     store.Store
	blobs  blob.Store
	lister blob.Lister
	leases Leases
	opts   Options
	mu     sync.Mutex // serialises runs
}

// New returns a Collector. blobs must implement blob.Lister. leases must be
// the same instance the upload path pins blobs with.
func New(st store.Store, blobs blob.Store, leases Leases, opts Options) (*Collector, error) {
	l, ok := blobs.(blob.Lister)
	if !ok {
		return nil, fmt.Errorf("gc: blob store %T cannot list blobs", blobs)
	}
	if st == nil || leases == nil {
		return nil, errors.New("gc: store and leases are required")
	}
	if opts.Grace == 0 {
		opts.Grace = DefaultGrace
	}
	if opts.Grace < 0 {
		opts.Grace = 0
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Collector{st: st, blobs: blobs, lister: l, leases: leases, opts: opts}, nil
}

// Result summarises one run.
type Result struct {
	DryRun bool
	// Stored is the number of blobs in the blob store; Rows the number of
	// blob rows in the database; Referenced the number of distinct digests
	// referenced by manifests.
	Stored, Rows, Referenced int
	// Young counts unreferenced stored blobs spared by the grace period.
	Young int
	// Candidates is the number of unreferenced blobs past the grace period.
	Candidates int
	// Deleted counts deleted blobs (in dry-run mode: blobs that would be),
	// DeletedBytes their stored size.
	Deleted      int
	DeletedBytes int64
	// InFlight counts candidates spared because an upload had pinned them;
	// Rereferenced those a commit referenced again before the sweep.
	InFlight, Rereferenced int
	// Failed counts candidates whose deletion failed; they are retried on
	// the next run.
	Failed int
}

type candidate struct {
	digest    string
	size      int64
	hasObject bool
}

// Collect performs one mark-and-sweep run. Per-blob failures are counted in
// Result.Failed and logged; an error is returned only when the run could
// not complete (listing failed, or ctx was cancelled).
func (c *Collector) Collect(ctx context.Context) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	start := time.Now()
	res, err := c.collect(ctx)
	if m := c.opts.Metrics; m != nil {
		m.ObserveGCRun(err, time.Since(start))
		deleted := observability.GCDeleted
		if res.DryRun {
			deleted = observability.GCDryRun
		}
		m.AddGCBlobs(deleted, res.Deleted, res.DeletedBytes)
		m.AddGCBlobs(observability.GCInFlight, res.InFlight, 0)
		m.AddGCBlobs(observability.GCReferenced, res.Rereferenced, 0)
		m.AddGCBlobs(observability.GCFailed, res.Failed, 0)
	}
	log := c.opts.Logger.With("dry_run", res.DryRun, "stored", res.Stored, "rows", res.Rows,
		"referenced", res.Referenced, "young", res.Young, "candidates", res.Candidates,
		"deleted", res.Deleted, "deleted_bytes", res.DeletedBytes, "in_flight", res.InFlight,
		"rereferenced", res.Rereferenced, "failed", res.Failed, "duration", time.Since(start))
	if err != nil {
		log.Error("blob gc failed", "error", err)
	} else {
		log.Info("blob gc finished")
	}
	return res, err
}

func (c *Collector) collect(ctx context.Context) (Result, error) {
	res := Result{DryRun: c.opts.DryRun}
	cutoff := c.opts.Now().Add(-c.opts.Grace)

	// Mark. Listing happens before reading references so that a blob
	// written and committed meanwhile is either listed young or found
	// referenced; either way the per-blob re-check below decides.
	objects := map[string]blob.Info{}
	err := c.lister.List(ctx, func(i blob.Info) error {
		objects[i.Digest.String()] = i
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("gc: list blobs: %w", err)
	}
	rows, err := c.st.ListBlobDigests(ctx)
	if err != nil {
		return res, fmt.Errorf("gc: list blob rows: %w", err)
	}
	refList, err := c.st.ListReferencedBlobDigests(ctx)
	if err != nil {
		return res, fmt.Errorf("gc: list blob references: %w", err)
	}
	refs := make(map[string]bool, len(refList))
	for _, d := range refList {
		refs[d] = true
	}
	res.Stored, res.Rows, res.Referenced = len(objects), len(rows), len(refs)

	var cands []candidate
	for d, i := range objects {
		switch {
		case refs[d]:
		case !i.ModTime.Before(cutoff):
			res.Young++
		default:
			cands = append(cands, candidate{digest: d, size: i.Size, hasObject: true})
		}
	}
	// Rows without a stored object (contents held inline, or the object is
	// already gone) have no age; the claim and re-check protect them.
	for _, d := range rows {
		if _, ok := objects[d]; !ok && !refs[d] {
			cands = append(cands, candidate{digest: d})
		}
	}
	slices.SortFunc(cands, func(a, b candidate) int { return cmp.Compare(a.digest, b.digest) })
	res.Candidates = len(cands)

	// Sweep.
	for _, cand := range cands {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		c.sweep(ctx, cand, &res)
	}
	return res, nil
}

// sweep deletes one candidate unless it is pinned or referenced again.
func (c *Collector) sweep(ctx context.Context, cand candidate, res *Result) {
	release, ok := c.leases.Claim(cand.digest)
	if !ok {
		res.InFlight++
		return
	}
	defer release()
	log := c.opts.Logger.With("digest", cand.digest)

	if c.opts.DryRun {
		ref, err := c.st.BlobReferenced(ctx, cand.digest)
		switch {
		case err != nil:
			res.Failed++
			log.Warn("blob gc: re-check failed", "error", err)
		case ref:
			res.Rereferenced++
		default:
			res.Deleted++
			res.DeletedBytes += cand.size
			log.Info("blob gc: would delete (dry run)", "size", cand.size)
		}
		return
	}

	// The row goes first, guarded by the reference check in the same
	// statement; if the object delete then fails, the next run finds the
	// object again and retries.
	err := c.st.DeleteBlob(ctx, cand.digest)
	if errors.Is(err, store.ErrConflict) {
		res.Rereferenced++
		return
	}
	if err != nil {
		res.Failed++
		log.Warn("blob gc: delete row failed", "error", err)
		return
	}
	if cand.hasObject {
		d, err := blob.ParseDigest(cand.digest)
		if err == nil {
			err = c.blobs.Delete(ctx, d)
		}
		if err != nil {
			res.Failed++
			log.Warn("blob gc: delete object failed", "error", err)
			return
		}
	}
	res.Deleted++
	res.DeletedBytes += cand.size
	log.Debug("blob gc: deleted", "size", cand.size)
}

// Leader decides whether this process collects. Lead returns ok=false when
// another process leads; release ends leadership for this run.
type Leader interface {
	Lead(ctx context.Context) (release func(), ok bool, err error)
}

// SingleProcess is the Leader for SQLite mode, where the database lock
// already guarantees a single server process: it always leads.
type SingleProcess struct{}

// Lead implements Leader.
func (SingleProcess) Lead(context.Context) (func(), bool, error) { return func() {}, true, nil }

// Run collects every interval until ctx is cancelled. Each run first asks
// leader (nil means SingleProcess) for leadership.
func (c *Collector) Run(ctx context.Context, interval time.Duration, leader Leader) {
	if leader == nil {
		leader = SingleProcess{}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		release, ok, err := leader.Lead(ctx)
		if err != nil {
			c.opts.Logger.Warn("blob gc: leader election failed", "error", err)
			continue
		}
		if !ok {
			continue
		}
		_, _ = c.Collect(ctx) // logged and counted by Collect
		release()
	}
}
