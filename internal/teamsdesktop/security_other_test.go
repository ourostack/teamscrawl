//go:build !windows

package teamsdesktop

import "testing"

func assertCurrentUserAndSystemOnly(t *testing.T, _ string) {
	t.Helper()
	t.Fatal("assertCurrentUserAndSystemOnly is Windows-only")
}
