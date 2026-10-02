package acceptance

import (
	"path/filepath"
	"runtime"
	"testing"
)

func diffScriptPath(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Join(filepath.Dir(file), "diff.mjs")
}

func diffScriptDir(t testing.TB) string {
	t.Helper()
	return filepath.Dir(diffScriptPath(t))
}
