// SPDX-License-Identifier: Apache-2.0

package module

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"ab", "api", "grpc-gateway", "v1", "a" + strings.Repeat("b", 99)} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a", "Api", "1api", "api-", "-api", "a_b", "a.b", "a/b", "a" + strings.Repeat("b", 100)} {
		if err := ValidateName(bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidateName(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
}

func TestValidateLabelName(t *testing.T) {
	for _, ok := range []string{"main", "v1.2.3", "feature/x", "a", "Release_1", strings.Repeat("x", 250), strings.Repeat("é", 250)} {
		if err := ValidateLabelName(ok); err != nil {
			t.Errorf("ValidateLabelName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "a\tb", "a\x00", "\xff", strings.Repeat("x", 251), strings.Repeat("é", 251)} {
		if err := ValidateLabelName(bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidateLabelName(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
}
