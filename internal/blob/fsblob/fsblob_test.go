// SPDX-License-Identifier: Apache-2.0

package fsblob_test

import (
	"testing"

	"github.com/marijnkoesen/protodex/internal/blob"
	"github.com/marijnkoesen/protodex/internal/blob/blobtest"
	"github.com/marijnkoesen/protodex/internal/blob/fsblob"
)

func TestContract(t *testing.T) {
	blobtest.Run(t, func(t *testing.T, limit int64) blob.Store {
		s, err := fsblob.New(t.TempDir(), limit)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
