//go:build !windows

package store

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAcquireLockFlockFailure(t *testing.T) {
	dir := t.TempDir()
	old := flock
	flock = func(int, int) error { return syscall.EIO }
	t.Cleanup(func() { flock = old })
	_, err := AcquireLock(filepath.Join(dir, "e.db"))
	codedAs(t, err, "db_error")
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("cause lost: %v", err)
	}
}
