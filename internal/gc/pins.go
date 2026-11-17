// SPDX-License-Identifier: Apache-2.0

package gc

import (
	"context"
	"sync"
)

// Leases coordinate garbage collection with writers that are about to
// reference blobs. A writer pins every blob digest it will reference before
// it writes the blob, and releases the pin only after the metadata
// referencing it is committed (or the write has failed). The collector
// claims a digest before it re-checks references and deletes; a claim fails
// while the digest is pinned, and pinning waits while it is claimed.
//
// Because every new reference is created under a pin, a blob the collector
// has claimed and found unreferenced stays unreferenced until the claim is
// released, so deleting it is safe.
//
// Pins is the in-process implementation, sufficient in SQLite mode where
// the database lock makes this process the only writer. A multi-replica
// (Postgres) deployment needs a database-backed implementation shared by
// all replicas.
type Leases interface {
	// Pin pins digests, waiting while any of them is claimed. The returned
	// release function must be called exactly once.
	Pin(ctx context.Context, digests []string) (release func(), err error)
	// Claim claims digest for deletion unless it is pinned or already
	// claimed. A successful claim must be ended with release.
	Claim(digest string) (release func(), ok bool)
}

// Pins is an in-process Leases.
type Pins struct {
	mu      sync.Mutex
	pinned  map[string]int
	claimed map[string]chan struct{} // closed when the claim ends
}

var _ Leases = (*Pins)(nil)

// NewPins returns an empty Pins.
func NewPins() *Pins {
	return &Pins{pinned: map[string]int{}, claimed: map[string]chan struct{}{}}
}

// Pin implements Leases.
func (p *Pins) Pin(ctx context.Context, digests []string) (func(), error) {
	for {
		p.mu.Lock()
		var wait chan struct{}
		for _, d := range digests {
			if ch, ok := p.claimed[d]; ok {
				wait = ch
				break
			}
		}
		if wait == nil {
			for _, d := range digests {
				p.pinned[d]++
			}
			p.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { p.unpin(digests) }) }, nil
		}
		p.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (p *Pins) unpin(digests []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range digests {
		if p.pinned[d]--; p.pinned[d] <= 0 {
			delete(p.pinned, d)
		}
	}
}

// Claim implements Leases.
func (p *Pins) Claim(digest string) (func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pinned[digest] > 0 {
		return nil, false
	}
	if _, ok := p.claimed[digest]; ok {
		return nil, false
	}
	ch := make(chan struct{})
	p.claimed[digest] = ch
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			delete(p.claimed, digest)
			p.mu.Unlock()
			close(ch)
		})
	}, true
}

// Pinned reports how many pins digest holds (for tests and debugging).
func (p *Pins) Pinned(digest string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pinned[digest]
}
