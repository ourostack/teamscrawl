//go:build !windows

package store

import "testing"

func assertCurrentUserAndSystemOnly(t *testing.T, _ string) {
	t.Helper()
	t.Fatal("assertCurrentUserAndSystemOnly is Windows-only")
}

func setCurrentUserAndSystemOnly(t *testing.T, _ string) {
	t.Helper()
}
