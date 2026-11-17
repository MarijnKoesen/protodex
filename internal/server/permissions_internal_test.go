// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// TestEveryProcedureHasAPolicy guards the default-deny policy table: every
// RPC of the vendored registry services must be listed, and nothing else.
func TestEveryProcedureHasAPolicy(t *testing.T) {
	seen := map[string]bool{authnAlphaGetCurrentUser: true}
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !strings.HasPrefix(string(fd.Package()), "buf.registry.") {
			return true
		}
		for i := range fd.Services().Len() {
			svc := fd.Services().Get(i)
			for j := range svc.Methods().Len() {
				p := "/" + string(svc.FullName()) + "/" + string(svc.Methods().Get(j).Name())
				seen[p] = true
				if _, ok := procedurePolicy[p]; !ok {
					t.Errorf("procedure %s has no authorization policy", p)
				}
			}
		}
		return true
	})
	for p := range procedurePolicy {
		if !seen[p] {
			t.Errorf("policy for unknown procedure %s", p)
		}
	}
	if len(seen) < 30 {
		t.Fatalf("only %d procedures registered; generated packages not linked?", len(seen))
	}
}
