//go:build !windows

package teamsdesktop

import "os"

func makeSnapshotRoot() (string, error) { return os.MkdirTemp("", snapshotPrefix) }

func makeSnapshotDir(path string) error { return os.MkdirAll(path, 0o700) }

func openSnapshotFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path is inside our private snapshot directory
}
