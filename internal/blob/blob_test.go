// SPDX-License-Identifier: Apache-2.0

package blob_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/marijnkoesen/protodex/internal/blob"
)

func TestParseDigest(t *testing.T) {
	good := blob.DigestOf([]byte("a")).String()
	if d, err := blob.ParseDigest(good); err != nil || d.String() != good {
		t.Fatalf("roundtrip: %v %v", d, err)
	}
	for _, bad := range []string{"", "sha256:", "md5:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("g", 64), "sha256:" + strings.Repeat("A", 64), "sha256:abc"} {
		if _, err := blob.ParseDigest(bad); !errors.Is(err, blob.ErrInvalidDigest) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}
