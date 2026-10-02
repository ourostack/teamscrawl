//go:build windows

package teamsdesktop

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestSnapshotCreatesPrivateRootBeforeCopyAndKeepsPrivateCopies(t *testing.T) {
	privateTempDir(t)

	var observedRoot string
	old := onSnapshotAttempt
	onSnapshotAttempt = func(attempt int) {
		if attempt != 1 {
			return
		}
		matches, err := filepath.Glob(filepath.Join(os.TempDir(), snapshotPrefix+"*"))
		if err != nil {
			t.Fatalf("glob snapshot roots: %v", err)
		}
		if len(matches) != 1 {
			t.Fatalf("roots = %v, want 1", matches)
		}
		observedRoot = matches[0]
		assertCurrentUserAndSystemOnly(t, observedRoot)
		if _, err := os.Stat(filepath.Join(observedRoot, "leveldb")); !os.IsNotExist(err) {
			t.Fatalf("leveldb existed before copy started: %v", err)
		}
	}
	t.Cleanup(func() { onSnapshotAttempt = old })

	snap, cleanup, err := Snapshot(context.Background(), fixtureSource(t))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if observedRoot == "" {
		t.Fatal("snapshot root was not observed before the copy")
	}
	if observedRoot != snap {
		t.Fatalf("observed root %q != snapshot %q", observedRoot, snap)
	}

	assertCurrentUserAndSystemOnly(t, snap)
	assertCurrentUserAndSystemOnly(t, filepath.Join(snap, "leveldb"))
	assertCurrentUserAndSystemOnly(t, filepath.Join(snap, "blob"))
	assertCurrentUserAndSystemOnly(t, filepath.Join(snap, "leveldb", "CURRENT"))
	assertCurrentUserAndSystemOnly(t, firstRegularFile(t, filepath.Join(snap, "leveldb")))
	assertCurrentUserAndSystemOnly(t, firstRegularFile(t, filepath.Join(snap, "blob")))
}

func firstRegularFile(t *testing.T, root string) string {
	t.Helper()
	var first string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if first == "" && d.Type().IsRegular() {
			first = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if first == "" {
		t.Fatalf("no regular files under %s", root)
	}
	return first
}

func assertCurrentUserAndSystemOnly(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("security info for %s: %v", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("dacl for %s: %v", path, err)
	}
	if dacl == nil {
		t.Fatalf("nil dacl for %s", path)
	}

	got := aceSIDStrings(t, dacl)
	want := map[string]struct{}{mustCurrentUserSID(t).String(): {}, mustSystemSID(t).String(): {}}
	if len(got) != len(want) {
		t.Fatalf("%s trustees = %v, want %v", path, got, keys(want))
	}
	for sid := range want {
		if _, ok := got[sid]; !ok {
			t.Fatalf("%s trustees = %v, want %v", path, got, keys(want))
		}
	}
}

func aceSIDStrings(t *testing.T, acl *windows.ACL) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	for i := uint16(0); i < acl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, uint32(i), &ace); err != nil {
			t.Fatalf("get ace %d: %v", i, err)
		}
		out[(*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()] = struct{}{}
	}
	return out
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustCurrentUserSID(t *testing.T) *windows.SID {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("token user: %v", err)
	}
	return user.User.Sid
}

func mustSystemSID(t *testing.T) *windows.SID {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("system sid: %v", err)
	}
	return sid
}
