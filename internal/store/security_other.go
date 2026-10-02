//go:build !windows

package store

func prepareArchiveForWrite(path string) error { return ensureParent(path) }

func finalizeArchiveFile(path string) error { return chmodFile(path, 0o600) }

func mapArchiveOpenError(err error) error { return err }
