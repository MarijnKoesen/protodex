// SPDX-License-Identifier: Apache-2.0

// Package blob defines a content-addressed blob store keyed by SHA-256
// digest, so identical content is stored once across commits and modules.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// DefaultMaxSize is the default upper bound on a single blob (5 GiB, the
// largest object S3 can server-side copy in one request).
const DefaultMaxSize int64 = 5 << 30

var (
	// ErrNotFound is returned when a blob does not exist.
	ErrNotFound = errors.New("blob: not found")
	// ErrDigestMismatch is returned by Put when the content does not hash to
	// the digest it was stored under. Nothing is stored in that case.
	ErrDigestMismatch = errors.New("blob: content does not match digest")
	// ErrTooLarge is returned by Put when the content exceeds the size limit.
	ErrTooLarge = errors.New("blob: content exceeds size limit")
	// ErrInvalidDigest is returned for malformed digests.
	ErrInvalidDigest = errors.New("blob: invalid digest")
)

// Digest is a SHA-256 content digest, written "sha256:<64 hex chars>".
type Digest struct{ hex string }

// ParseDigest parses the "sha256:<hex>" form.
func ParseDigest(s string) (Digest, error) {
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(h) != sha256.Size*2 || h != strings.ToLower(h) {
		return Digest{}, fmt.Errorf("%w: %q", ErrInvalidDigest, s)
	}
	if _, err := hex.DecodeString(h); err != nil {
		return Digest{}, fmt.Errorf("%w: %q", ErrInvalidDigest, s)
	}
	return Digest{hex: h}, nil
}

// DigestOf returns the digest of data.
func DigestOf(data []byte) Digest {
	sum := sha256.Sum256(data)
	return Digest{hex: hex.EncodeToString(sum[:])}
}

// Hex returns the lowercase hex encoding without the algorithm prefix.
func (d Digest) Hex() string { return d.hex }

// IsZero reports whether d is the zero (invalid) digest.
func (d Digest) IsZero() bool { return d.hex == "" }

// String returns the "sha256:<hex>" form.
func (d Digest) String() string { return "sha256:" + d.hex }

// Store is a content-addressed blob store. Implementations are safe for
// concurrent use and never buffer whole blobs in memory.
type Store interface {
	// Put stores the content of r under d. The content is hashed while it
	// streams; if it does not match d, or exceeds the size limit, nothing
	// becomes visible and ErrDigestMismatch or ErrTooLarge is returned.
	// Putting an existing digest is a no-op success.
	Put(ctx context.Context, d Digest, r io.Reader) error
	// Get returns a stream of the blob, or ErrNotFound.
	Get(ctx context.Context, d Digest) (io.ReadCloser, error)
	// Exists reports whether the blob is present.
	Exists(ctx context.Context, d Digest) (bool, error)
	// Delete removes the blob. Deleting a missing blob is not an error.
	Delete(ctx context.Context, d Digest) error
}

// Info describes a stored blob.
type Info struct {
	Digest Digest
	Size   int64
	// ModTime is when the blob was last written. Re-putting an existing
	// blob is a no-op, so it does not refresh ModTime.
	ModTime time.Time
}

// Lister is implemented by stores that can enumerate their blobs, which
// garbage collection needs.
type Lister interface {
	// List calls fn for every blob, in no particular order. Blobs put or
	// deleted while List runs may or may not be reported. An error from fn
	// stops the walk and is returned.
	List(ctx context.Context, fn func(Info) error) error
}

// Verifier wraps a reader, hashing and counting bytes and failing with
// ErrTooLarge as soon as more than limit bytes are read.
type Verifier struct {
	r     io.Reader
	h     hashWriter
	limit int64
	n     int64
}

type hashWriter interface {
	io.Writer
	Sum(b []byte) []byte
}

// NewVerifier returns a Verifier limited to limit bytes (limit <= 0 selects
// DefaultMaxSize).
func NewVerifier(r io.Reader, limit int64) *Verifier {
	if limit <= 0 {
		limit = DefaultMaxSize
	}
	return &Verifier{r: r, h: sha256.New(), limit: limit}
}

// Read implements io.Reader.
func (v *Verifier) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	v.n += int64(n)
	if v.n > v.limit {
		return 0, ErrTooLarge
	}
	_, _ = v.h.Write(p[:n]) // hash.Hash never fails
	return n, err
}

// Size returns the number of bytes read so far.
func (v *Verifier) Size() int64 { return v.n }

// Check returns ErrDigestMismatch unless everything read hashes to d.
func (v *Verifier) Check(d Digest) error {
	if hex.EncodeToString(v.h.Sum(nil)) != d.hex {
		return ErrDigestMismatch
	}
	return nil
}
