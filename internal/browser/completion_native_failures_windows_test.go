//go:build windows

package browser

import (
	"errors"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCompletionNativeJobRejectsInvalidLimitAndIdentity(t *testing.T) {
	f := newCompletionNativeFixture(t)
	for _, limit := range []int{0, -1, 4097} {
		if _, err := jobPIDs(7, limit); err == nil || err.Error() != "browser_completion_too_large" {
			t.Fatalf("invalid bound must refuse before native query: %v", err)
		}
	}
	f.jobPids = []uint32{0}
	if _, err := jobPIDs(7, 4096); err == nil || err.Error() != "browser_completion_identity_failed" {
		t.Fatalf("zero native identity admitted: %v", err)
	}
}

func TestCompletionNativeCreationRejectsFailedAndZeroTimes(t *testing.T) {
	old := completionGetProcessTimes
	t.Cleanup(func() { completionGetProcessTimes = old })
	cause := errors.New("private time query")
	for _, failed := range []bool{true, false} {
		completionGetProcessTimes = func(windows.Handle, *windows.Filetime, *windows.Filetime, *windows.Filetime, *windows.Filetime) error {
			if failed {
				return cause
			}
			return nil
		}
		created, err := completionCreationTime(42)
		if created != 0 || err == nil || (failed && !errors.Is(err, cause)) {
			t.Fatalf("unqualified creation time: %d %v", created, err)
		}
	}
}

func TestCompletionNativeMembershipRejectsInvalidProcessHandle(t *testing.T) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.CloseHandle(job); err != nil {
			t.Error(err)
		}
	}()
	if owned, err := completionInJob(0, job); owned || err == nil {
		t.Fatalf("invalid process cannot establish private-job ownership: %v %v", owned, err)
	}
}

func TestCompletionNativeArgvRejectsExpiredAndShortReturnedQuery(t *testing.T) {
	old := completionQueryProcess
	t.Cleanup(func() { completionQueryProcess = old })
	calls := 0
	completionQueryProcess = func(_ windows.Handle, _ int32, ptr unsafe.Pointer, _ uint32, n *uint32) error {
		calls++
		*n = 32
		if ptr != nil {
			*n = 0
		}
		return nil
	}
	if _, err := readProcArgsHandle(42, 65536, time.Now().Add(-time.Second)); err == nil || calls != 0 {
		t.Fatalf("expired acquisition must not query: %v calls=%d", err, calls)
	}
	if _, err := readProcArgsHandle(42, 65536, time.Now().Add(time.Minute)); err == nil ||
		err.Error() != "browser_completion_args_unqualified" || calls != 2 {
		t.Fatalf("short second query must refuse before header access: %v calls=%d", err, calls)
	}
}

func TestCompletionNativeTerminationFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		wantKill   int
		wantClose  int
	}{
		{"expired-entry", "browser_completion_timeout", 0, 0},
		{"late-open-error", "browser_completion_terminate_open_failed", 0, 0},
		{"ended-before-open-error", "", 0, 0},
		{"release-error", "browser_completion_release_failed", 1, 1},
		{"late-creation", "browser_completion_timeout", 0, 1},
		{"ended-at-recheck", "", 0, 1},
		{"late-recheck", "browser_completion_timeout", 0, 1},
		{"late-termination-error", "browser_completion_terminate_failed", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCompletionNativeFixture(t)
			deadline := time.Now().Add(time.Minute)
			if tc.name == "late-open-error" || tc.name == "late-creation" || tc.name == "late-recheck" || tc.name == "late-termination-error" {
				deadline = time.Now().Add(20 * time.Millisecond)
			}
			cause := errors.New("private native failure")
			consume := func() { time.Sleep(time.Until(deadline) + time.Millisecond) }
			switch tc.name {
			case "expired-entry":
				deadline = time.Now().Add(-time.Second)
			case "late-open-error", "ended-before-open-error":
				completionOpenProcess = func(uint32, bool, uint32) (windows.Handle, error) {
					if tc.name == "late-open-error" {
						consume()
					} else {
						f.signalled[42] = true
					}
					return 0, cause
				}
			case "release-error":
				completionCloseHandle = func(h windows.Handle) error { f.closed[h]++; return cause }
			case "late-creation":
				f.onCreation = func(uint32) { consume() }
			case "ended-at-recheck", "late-recheck":
				f.onPoll = func(pid uint32) {
					if f.polls[pid] == 2 {
						if tc.name == "ended-at-recheck" {
							f.signalled[pid] = true
						} else {
							consume()
						}
					}
				}
			case "late-termination-error":
				completionTerminateProcess = func(windows.Handle, uint32) error {
					f.fallbackKills++
					consume()
					return cause
				}
			}
			err := terminateCompletionTarget(42, 42, 142, deadline)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != tc.code {
				t.Fatalf("wrong fixed failure: %v; want %s", err, tc.code)
			}
			if f.fallbackKills != tc.wantKill || f.closed[42] != tc.wantClose {
				t.Fatalf("refused work or transient disposal: kills=%d closes=%d", f.fallbackKills, f.closed[42])
			}
			if tc.name == "late-open-error" || tc.name == "release-error" || tc.name == "late-termination-error" {
				if !errors.Is(err, cause) {
					t.Fatal("native cause was lost")
				}
			}
		})
	}
}
