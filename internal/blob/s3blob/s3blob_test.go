// SPDX-License-Identifier: Apache-2.0

package s3blob_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/google/uuid"

	"github.com/marijnkoesen/protodex/internal/blob"
	"github.com/marijnkoesen/protodex/internal/blob/blobtest"
	"github.com/marijnkoesen/protodex/internal/blob/s3blob"
)

// TestContractFake runs the contract against an in-process fake S3.
func TestContractFake(t *testing.T) {
	srv := newFakeS3(t)
	blobtest.Run(t, func(t *testing.T, limit int64) blob.Store {
		c := s3blob.NewClient(srv.URL, "us-east-1", "k", "s", true, 2)
		return s3blob.New(s3blob.Config{Client: c, Bucket: "b", Prefix: t.Name() + "/", MaxSize: limit, PartSize: 5 << 20})
	})
}

// TestContractReal runs the contract against a real S3-compatible server
// (e.g. SeaweedFS or MinIO) when PROTODEX_TEST_S3_ENDPOINT is set. Also needs
// PROTODEX_TEST_S3_BUCKET, _ACCESS_KEY and _SECRET_KEY; the bucket is created if missing.
func TestContractReal(t *testing.T) {
	endpoint := os.Getenv("PROTODEX_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("PROTODEX_TEST_S3_ENDPOINT not set")
	}
	bucket := os.Getenv("PROTODEX_TEST_S3_BUCKET")
	c := s3blob.NewClient(endpoint, "us-east-1",
		os.Getenv("PROTODEX_TEST_S3_ACCESS_KEY"), os.Getenv("PROTODEX_TEST_S3_SECRET_KEY"), true, 3)
	// Create the bucket if needed (works on any S3-compatible server).
	if _, err := c.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if !errors.As(err, &owned) && !errors.As(err, &exists) {
			t.Fatalf("create bucket: %v", err)
		}
	}
	blobtest.Run(t, func(_ *testing.T, limit int64) blob.Store {
		return s3blob.New(s3blob.Config{Client: c, Bucket: bucket, Prefix: "test/" + uuid.NewString() + "/", MaxSize: limit})
	})
}
