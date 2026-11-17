// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"connectrpc.com/connect"

	"github.com/marijnkoesen/protodex/internal/store"
)

func TestTopoSortCycle(t *testing.T) {
	mk := func(id string) depCommit { return depCommit{commit: store.Commit{ID: id}} }
	cs := []depCommit{mk("a"), mk("b"), mk("c")}
	if _, err := topoSort(cs, [][2]string{{"a", "b"}, {"b", "c"}}); err != nil {
		t.Fatal(err)
	}
	_, err := topoSort(cs, [][2]string{{"a", "b"}, {"b", "c"}, {"c", "b"}})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v", err)
	}
}
