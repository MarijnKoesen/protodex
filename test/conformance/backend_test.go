// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/marijnkoesen/protodex/internal/store"
	"github.com/marijnkoesen/protodex/internal/store/sqlite"
)

// Backend is a metadata store the suite can run protodex on. A new backend
// (e.g. Postgres, #23) registers itself from an init function in a
// backend_<name>_test.go file; Skip lets it opt out where its database is
// not configured.
type Backend struct {
	Name string
	// Skip returns why the backend cannot run here, or "" if it can.
	Skip func() string
	// Open returns an empty, migrated store private to one test. dir is a
	// fresh temporary directory the backend may use. The suite closes the
	// store when the test ends.
	Open func(ctx context.Context, dir string) (store.Store, error)
	// Remote marks the backend that targets an already running server
	// (PROTODEX_CONFORMANCE_REMOTE, see remote_test.go) instead of opening a
	// store; Open is unused.
	Remote bool
}

var backends []Backend

func registerBackend(b Backend) { backends = append(backends, b) }

func init() {
	registerBackend(Backend{
		Name: "sqlite",
		Open: func(ctx context.Context, dir string) (store.Store, error) {
			st, err := sqlite.Open(filepath.Join(dir, "protodex.db"))
			if err != nil {
				return nil, err
			}
			if err := st.Migrate(ctx); err != nil {
				_ = st.Close()
				return nil, err
			}
			return st, nil
		},
	})
}

// selectedBackends returns the registered backends named in
// PROTODEX_CONFORMANCE_BACKENDS (all when unset), with a skip reason each.
func selectedBackends() []Backend {
	want := os.Getenv("PROTODEX_CONFORMANCE_BACKENDS")
	var out []Backend
	for _, b := range backends {
		if want != "" && !slices.Contains(strings.Split(want, ","), b.Name) {
			continue
		}
		out = append(out, b)
	}
	return out
}
