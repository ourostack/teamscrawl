//go:build windows

package browser

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestCompletionProductionDeniedOutcomesDoNotEraseIndependentFailure(t *testing.T) {
	for _, stage := range []string{"non-denied", "prior-error", "one-of-two", "late-denial"} {
		t.Run(stage, func(t *testing.T) {
			f := newCompletionNativeFixture(t)
			f.w.set.deadline = time.Now().Add(time.Minute)
			realCause := errors.New("synthetic independent failure")
			f.snapshotPids = []uint32{42}
			f.owned[42] = true
			if stage == "one-of-two" {
				f.snapshotPids = []uint32{42, 43}
				f.owned[43] = true
			}
			if stage == "late-denial" {
				f.snapshotPids = nil
				f.onSnapshot = func() {
					if f.snapshots == 3 {
						f.snapshotPids = []uint32{42}
					}
				}
			} else if err := discoverCompletionTargets(f.w); err != nil {
				t.Fatal(err)
			}
			f.afterKill = func() { f.snapshotPids = nil }
			if stage == "prior-error" {
				f.w.firstErr = &completionFailure{code: "browser_completion_identity_failed", cause: realCause}
			}
			completionTerminateProcess = func(windows.Handle, uint32) error {
				f.fallbackKills++
				if stage == "non-denied" {
					return realCause
				}
				return windows.ERROR_ACCESS_DENIED
			}
			f.onPoll = func(pid uint32) {
				if stage == "late-denial" {
					return
				}
				if f.polls[pid] >= 5 {
					if stage != "one-of-two" || pid == 42 {
						f.signalled[pid] = true
					} else {
						f.w.set.deadline = time.Now().Add(-time.Second)
					}
				}
			}
			err := f.w.stop(nil)
			wantCode := "browser_completion_terminate_failed"
			if stage == "prior-error" {
				wantCode = "browser_completion_identity_failed"
			} else if stage == "late-denial" {
				wantCode = "browser_completion_late_process"
			}
			if err == nil || err.Error() != wantCode {
				t.Fatalf("independent or unresolved outcome erased: %v want=%s", err, wantCode)
			}
			if (stage == "non-denied" || stage == "prior-error") && !errors.Is(err, realCause) {
				t.Fatal("independent cause was dropped")
			}
			if (stage == "one-of-two" || stage == "late-denial") && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Fatal("unresolved native denial cause was dropped")
			}
			if stage == "late-denial" && (f.polls[42] != 5 || f.fallbackKills != 1) {
				t.Fatalf("late denial added a wait or termination retry: polls=%v kills=%d", f.polls, f.fallbackKills)
			}
			for _, pid := range []windows.Handle{42, 43} {
				if f.opened[pid] != f.closed[pid] {
					t.Fatalf("generation disposal differs from acquisition: opened=%v closed=%v", f.opened, f.closed)
				}
			}
			if f.closed[7] != 1 {
				t.Fatal("local job was not disposed exactly once")
			}
		})
	}
}
