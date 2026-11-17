// SPDX-License-Identifier: Apache-2.0

// Package s3blob is an S3-compatible blob.Store (AWS S3, MinIO, GCS interop).
//
// Uploads stream through the SDK's multipart uploader (bounded memory:
// PartSize x Concurrency) into a staging key, are digest-verified, and are
// then server-side copied to their final key, so a bad upload can never
// replace valid content. The copy limits blobs to 5 GiB.
package s3blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"

	"github.com/marijnkoesen/protodex/internal/blob"
)

// Config configures a Store.
type Config struct {
	Client *s3.Client
	Bucket string
	// Prefix is prepended to all keys (e.g. "blobs/").
	Prefix string
	// MaxSize bounds a single blob; <= 0 selects blob.DefaultMaxSize.
	MaxSize int64
	// PartSize is the multipart part size; <= 0 selects 8 MiB.
	PartSize int64
	// Concurrency is the number of parallel part uploads; <= 0 selects 2.
	Concurrency int
}

// NewClient builds an S3 client for a custom endpoint (MinIO, GCS interop).
// An empty endpoint targets AWS. pathStyle selects path-style addressing.
// Retries use the SDK's standard retryer with the given max attempts.
func NewClient(endpoint, region, accessKey, secretKey string, pathStyle bool, maxAttempts int) *s3.Client {
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	opts := s3.Options{
		Region:           region,
		UsePathStyle:     pathStyle,
		RetryMaxAttempts: maxAttempts,
		// GCS interop and older MinIO reject the SDK's default trailing checksums.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Credentials:                aws.NewCredentialsCache(staticCreds{accessKey, secretKey}),
	}
	if endpoint != "" {
		opts.BaseEndpoint = aws.String(endpoint)
	}
	return s3.New(opts)
}

type staticCreds struct{ id, secret string }

func (c staticCreds) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: c.id, SecretAccessKey: c.secret}, nil
}

// Store implements blob.Store on S3.
type Store struct {
	cfg Config
	up  *manager.Uploader //nolint:staticcheck // transfermanager is not yet stable
}

var _ blob.Store = (*Store)(nil)

// New returns a Store.
func New(cfg Config) *Store {
	if cfg.PartSize <= 0 {
		cfg.PartSize = 8 << 20
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.MaxSize <= 0 || cfg.MaxSize > blob.DefaultMaxSize {
		cfg.MaxSize = blob.DefaultMaxSize
	}
	up := manager.NewUploader(cfg.Client, func(u *manager.Uploader) { //nolint:staticcheck
		u.PartSize = cfg.PartSize
		u.Concurrency = cfg.Concurrency
	})
	return &Store{cfg: cfg, up: up}
}

func (s *Store) key(d blob.Digest) (string, error) {
	if d.IsZero() {
		return "", blob.ErrInvalidDigest
	}
	return s.cfg.Prefix + "sha256/" + d.Hex()[:2] + "/" + d.Hex(), nil
}

func notFound(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return true
		}
	}
	var he interface{ HTTPStatusCode() int }
	return errors.As(err, &he) && he.HTTPStatusCode() == http.StatusNotFound
}

// Put implements blob.Store.
func (s *Store) Put(ctx context.Context, d blob.Digest, r io.Reader) error {
	key, err := s.key(d)
	if err != nil {
		return err
	}
	if ok, err := s.Exists(ctx, d); err != nil {
		return err
	} else if ok {
		return nil
	}
	staging := s.cfg.Prefix + "tmp/" + uuid.NewString()
	v := blob.NewVerifier(&ctxReader{ctx, r}, s.cfg.MaxSize)
	// Upload runs detached from ctx's cancellation (ctxReader surfaces it as a
	// read error instead) so the SDK's multipart abort is not itself cancelled
	// and incomplete parts do not leak.
	upCtx, cancelUp := context.WithTimeout(context.WithoutCancel(ctx), uploadTimeout)
	defer cancelUp()
	_, err = s.up.Upload(upCtx, &s3.PutObjectInput{ //nolint:staticcheck
		Bucket: &s.cfg.Bucket, Key: &staging, Body: v,
	})
	defer func() {
		// Best effort, detached so a cancelled ctx still cleans up.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_, _ = s.cfg.Client.DeleteObject(cctx,
			&s3.DeleteObjectInput{Bucket: &s.cfg.Bucket, Key: &staging})
	}()
	if err != nil {
		// Surface our sentinel errors rather than the SDK's wrapping.
		if errors.Is(err, blob.ErrTooLarge) {
			return blob.ErrTooLarge
		}
		return fmt.Errorf("s3blob: upload: %w", err)
	}
	if err := v.Check(d); err != nil {
		return err
	}
	_, err = s.cfg.Client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     &s.cfg.Bucket,
		Key:        &key,
		CopySource: aws.String(url.PathEscape(s.cfg.Bucket) + "/" + escapeKey(staging)),
	})
	if err != nil {
		return fmt.Errorf("s3blob: copy: %w", err)
	}
	return nil
}

// Get implements blob.Store.
func (s *Store) Get(ctx context.Context, d blob.Digest) (io.ReadCloser, error) {
	key, err := s.key(d)
	if err != nil {
		return nil, err
	}
	out, err := s.cfg.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.cfg.Bucket, Key: &key})
	if err != nil {
		if notFound(err) {
			return nil, blob.ErrNotFound
		}
		return nil, fmt.Errorf("s3blob: get: %w", err)
	}
	return out.Body, nil
}

// Exists implements blob.Store.
func (s *Store) Exists(ctx context.Context, d blob.Digest) (bool, error) {
	key, err := s.key(d)
	if err != nil {
		return false, err
	}
	_, err = s.cfg.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.cfg.Bucket, Key: &key})
	if err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("s3blob: head: %w", err)
	}
	return true, nil
}

// Delete implements blob.Store.
func (s *Store) Delete(ctx context.Context, d blob.Digest) error {
	key, err := s.key(d)
	if err != nil {
		return err
	}
	_, err = s.cfg.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.cfg.Bucket, Key: &key})
	if err != nil && !notFound(err) {
		return fmt.Errorf("s3blob: delete: %w", err)
	}
	return nil
}

const (
	cleanupTimeout = 30 * time.Second
	uploadTimeout  = 24 * time.Hour
)

// escapeKey URL-encodes each path segment, as required for CopySource.
func escapeKey(k string) string {
	parts := strings.Split(k, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
