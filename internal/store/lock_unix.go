//go:build !windows

package store

import (
	"errors"
	"os"
	"sync"
	"syscall"

	"github.com/ourostack/teamscrawl/internal/errs"
)

// flock is syscall.Flock; tests replace it to force a failure other than contention.
var flock = syscall.Flock

// AcquireLock takes the exclusive, non-blocking run lock on <dbPath>.lock. A second holder gets
// the coded `locked` error at once. The lock is also released if the process dies. release is
// idempotent.
func AcquireLock(dbPath string) (release func(), err error) {
	if err := ensureParent(dbPath); err != nil {
		return nil, errs.DBError(err)
	}
	f, err := os.OpenFile(lockPath(dbPath), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: the caller chose the archive path
	if err != nil {
		return nil, errs.DBError(err)
	}
	if err := flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errs.Locked("another teamscrawl run holds the archive lock " + lockPath(dbPath))
		}
		return nil, errs.DBError(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}, nil
}
