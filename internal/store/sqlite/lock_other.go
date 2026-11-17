// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package sqlite

import "os"

// acquireLock is a no-op where flock is unavailable; single-process use is
// then the operator's responsibility.
func acquireLock(string) (*os.File, error) { return nil, nil }
