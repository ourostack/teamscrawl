//go:build windows

package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/ourostack/teamscrawl/internal/errs"
)

func TestAcquireLockWindowsContention(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sub", "archive.db")
	release1, err := AcquireLock(db)
	if err != nil {
		t.Fatal(err)
	}
	defer release1()

	_, err = AcquireLock(db)
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.CodeLocked {
		t.Fatalf("second lock: %v", err)
	}
}

func TestAcquireLockWindowsReleaseUnlocks(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sub", "archive.db")
	release1, err := AcquireLock(db)
	if err != nil {
		t.Fatal(err)
	}
	release1()
	release1()

	release2, err := AcquireLock(db)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	release2()
}
