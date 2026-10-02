//go:build windows

package store

import (
	"errors"
	"os"
	"sync"

	"github.com/ourostack/teamscrawl/internal/errs"
	"golang.org/x/sys/windows"
)

// AcquireLock takes the exclusive, non-blocking run lock on <dbPath>.lock. A second holder gets
// the coded `locked` error at once. The lock is also released if the process dies. release is
// idempotent.
func AcquireLock(dbPath string) (release func(), err error) {
	if err := prepareArchiveDir(dbPath); err != nil {
		return nil, errs.DBError(err)
	}
	f, err := os.OpenFile(lockPath(dbPath), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: the caller chose the archive path
	if err != nil {
		return nil, errs.DBError(err)
	}
	if err := secureLockHandle(f); err != nil {
		_ = f.Close()
		return nil, errs.DBError(err)
	}
	if err := lockFileEx(f); err != nil {
		_ = f.Close()
		if isLockContention(err) {
			return nil, errs.Locked("another teamscrawl run holds the archive lock " + lockPath(dbPath))
		}
		return nil, errs.DBError(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unlockFileEx(f)
			_ = f.Close()
		})
	}, nil
}

func lockFileEx(f *os.File) error {
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		^uint32(0),
		^uint32(0),
		&windows.Overlapped{},
	)
}

func unlockFileEx(f *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		^uint32(0),
		^uint32(0),
		&windows.Overlapped{},
	)
}

func isLockContention(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
