//go:build windows

package browser

import (
	"errors"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCompletionNativeAdmissionFailureDisposesAcquisitions(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		wantClose  int
	}{
		{"expired-entry", "browser_completion_timeout", 0},
		{"release-unrelated", "browser_completion_release_failed", 1},
		{"late-open", "browser_completion_timeout", 1},
		{"late-membership", "browser_completion_timeout", 1},
		{"late-args", "browser_completion_timeout", 1},
		{"late-path", "browser_completion_timeout", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCompletionNativeFixture(t)
			oldPath := completionLongPath
			t.Cleanup(func() { completionLongPath = oldPath })
			deadline := time.Now().Add(time.Minute)
			if tc.name == "late-open" || tc.name == "late-membership" || tc.name == "late-args" || tc.name == "late-path" {
				deadline = time.Now().Add(20 * time.Millisecond)
			}
			consume := func() { time.Sleep(time.Until(deadline) + time.Millisecond) }
			cause := errors.New("private release failure")
			switch tc.name {
			case "expired-entry":
				deadline = time.Now().Add(-time.Second)
			case "release-unrelated":
				completionCloseHandle = func(h windows.Handle) error { f.closed[h]++; return cause }
			case "late-open":
				completionOpenProcess = func(uint32, bool, uint32) (windows.Handle, error) {
					f.opened[42]++
					consume()
					return 42, nil
				}
				f.onCreation = func(uint32) { t.Fatal("creation query after open exhausted deadline") }
			case "late-membership":
				completionMembership = func(windows.Handle, windows.Handle) (bool, error) { consume(); return false, nil }
				completionReadArgs = func(windows.Handle, uint32, time.Time) ([]string, error) {
					t.Fatal("argv query after membership exhausted deadline")
					return nil, nil
				}
			case "late-args":
				completionReadArgs = func(windows.Handle, uint32, time.Time) ([]string, error) {
					consume()
					return []string{`--user-data-dir=C:\synthetic`}, nil
				}
				completionLongPath = func(string) string { t.Fatal("path query after argv exhausted deadline"); return "" }
			case "late-path":
				f.owned[42] = true
				completionLongPath = func(p string) string { consume(); return p }
			}
			target, err := openCompletionTarget(42, 7, `C:\synthetic`, false, deadline)
			if target != nil || err == nil || err.Error() != tc.code || f.closed[42] != tc.wantClose {
				t.Fatalf("failed admission/disposal: target=%v err=%v closes=%d", target != nil, err, f.closed[42])
			}
			if tc.name == "release-unrelated" && !errors.Is(err, cause) {
				t.Fatal("release cause was lost")
			}
		})
	}
}

func TestCompletionOwnedArgsStopsBeforeExpiredArgument(t *testing.T) {
	old := completionLongPath
	t.Cleanup(func() { completionLongPath = old })
	completionLongPath = func(string) string { t.Fatal("expired argument must not query path"); return "" }
	if owned, err := ownedCompletionArgs([]string{`--user-data-dir=C:\synthetic`}, `C:\synthetic`, time.Now().Add(-time.Second)); owned ||
		err == nil || err.Error() != "browser_completion_timeout" {
		t.Fatalf("expired ownership decision: %v %v", owned, err)
	}
}

func TestCompletionNativeDiscoveryFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		wantCloses int
	}{
		{"expired-entry", "browser_completion_timeout", 0},
		{"late-job-list", "browser_completion_timeout", 0},
		{"snapshot-close", "browser_completion_release_failed", 1},
		{"late-first-entry", "browser_completion_timeout", 1},
		{"enumeration-error", "browser_completion_snapshot_failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCompletionNativeFixture(t)
			f.w.set.deadline = time.Now().Add(time.Minute)
			if tc.name == "late-job-list" || tc.name == "late-first-entry" {
				f.w.set.deadline = time.Now().Add(20 * time.Millisecond)
			}
			deadline := f.w.set.deadline
			cause := errors.New("private snapshot operation")
			consume := func() { time.Sleep(time.Until(deadline) + time.Millisecond) }
			switch tc.name {
			case "expired-entry":
				f.w.set.deadline = time.Now().Add(-time.Second)
			case "late-job-list":
				f.jobPids = []uint32{42}
				query := completionQueryJob
				completionQueryJob = func(h windows.Handle, c int32, p unsafe.Pointer, n uint32, r *uint32) error {
					err := query(h, c, p, n, r)
					consume()
					return err
				}
				completionOpenProcess = func(uint32, bool, uint32) (windows.Handle, error) {
					t.Fatal("expired job list must not acquire targets")
					return 0, nil
				}
			case "snapshot-close":
				completionCloseHandle = func(h windows.Handle) error { f.closed[h]++; return cause }
			case "late-first-entry":
				f.snapshotPids = []uint32{42}
				f.onEnumeration = consume
				completionOpenProcess = func(uint32, bool, uint32) (windows.Handle, error) {
					t.Fatal("expired enumeration must not acquire targets")
					return 0, nil
				}
			case "enumeration-error":
				completionProcessFirst = func(windows.Handle, *windows.ProcessEntry32) error { return cause }
			}
			err := discoverCompletionTargets(f.w)
			if err == nil || err.Error() != tc.code || f.closed[99] != tc.wantCloses {
				t.Fatalf("discovery failure/disposal: %v closes=%d", err, f.closed[99])
			}
			if (tc.name == "snapshot-close" || tc.name == "enumeration-error") && !errors.Is(err, cause) {
				t.Fatal("native discovery cause was lost")
			}
		})
	}
}
