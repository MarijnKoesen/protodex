// SPDX-License-Identifier: Apache-2.0

//go:build unix

package sqlite

import (
	"fmt"
	"os"
	"syscall"
)

const errLockedMsg = "database is in use by another process"

func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits int
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("%w: %s", ErrLocked, errLockedMsg)
		}
		return nil, err
	}
	return f, nil
}
