//go:build windows

package browser

import (
	"errors"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCompletionProductionWitnessFailureDisposesEveryHandle(t *testing.T) {
	for _, tc := range []struct {
		name, code string
	}{
		{"job-release", "browser_completion_release_failed"},
		{"expired-entry", "browser_completion_timeout"},
		{"job-termination", "browser_completion_job_terminate_failed"},
		{"held-poll", "browser_completion_wait_failed"},
		{"missing-job", "browser_completion_wait_failed"},
		{"accounting-query", "browser_completion_wait_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCompletionNativeFixture(t)
			f.w.set.deadline = time.Now().Add(time.Minute)
			cause := errors.New("private completion boundary")
			switch tc.name {
			case "job-release":
				completionCloseHandle = func(h windows.Handle) error {
					f.closed[h]++
					if h == 7 {
						return cause
					}
					return nil
				}
			case "expired-entry":
				f.w.set.deadline = time.Now().Add(-time.Second)
			case "job-termination":
				completionTerminateJob = func(windows.Handle, uint32) error { f.jobKills++; return cause }
			case "held-poll":
				f.jobPids = []uint32{42}
				completionWaitProcess = func(windows.Handle, uint32) (uint32, error) { return windows.WAIT_FAILED, cause }
			case "missing-job":
				f.w.job = 0
			case "accounting-query":
				query := completionQueryJob
				completionQueryJob = func(h windows.Handle, c int32, p unsafe.Pointer, n uint32, r *uint32) error {
					if c == jobObjectBasicAccountingInformation {
						return cause
					}
					return query(h, c, p, n, r)
				}
			}
			err := f.w.stop(nil)
			if err == nil || err.Error() != tc.code || len(f.w.set.targets) != 0 || f.w.job != 0 {
				t.Fatalf("failure must preserve fixed outcome and release witness: %v", err)
			}
			if tc.name != "missing-job" && f.closed[7] != 1 {
				t.Fatalf("local job must dispose exactly once: %v", f.closed)
			}
			if tc.name == "held-poll" && f.closed[42] != f.opened[42] {
				t.Fatalf("held and duplicate acquisitions leaked: opened=%v closed=%v", f.opened, f.closed)
			}
			if tc.name == "expired-entry" && (f.jobKills != 0 || f.snapshots != 0) {
				t.Fatal("expired witness began native work")
			}
			if tc.name != "expired-entry" && tc.name != "missing-job" && !errors.Is(err, cause) {
				t.Fatal("private native cause was lost")
			}
		})
	}
}

func TestCompletionProductionWitnessChecksDeadlineBeforeAccounting(t *testing.T) {
	f := newCompletionNativeFixture(t)
	f.w.set.deadline = time.Now().Add(time.Minute)
	old := completionWindowsNow
	t.Cleanup(func() { completionWindowsNow = old })
	enteredPoll := false
	f.jobPids = []uint32{42}
	f.afterKill = func() { f.jobPids = nil }
	f.onPoll = func(pid uint32) { enteredPoll = true; f.signalled[pid] = true }
	before, deadline := time.Now(), f.w.set.deadline
	completionWindowsNow = func() time.Time {
		if enteredPoll {
			return deadline
		}
		return before
	}
	query := completionQueryJob
	accounting := 0
	completionQueryJob = func(h windows.Handle, c int32, p unsafe.Pointer, n uint32, r *uint32) error {
		if c == jobObjectBasicAccountingInformation {
			accounting++
		}
		return query(h, c, p, n, r)
	}
	err := f.w.stop(nil)
	if err == nil || accounting != 0 || f.closed[42] != 1 || f.closed[7] != 1 {
		t.Fatalf("late accounting work or leaked lifetime: err=%v accounting=%d closed=%v", err, accounting, f.closed)
	}
}

func TestCompletionProductionPrepareExpiredBudgetDoesNotAcquireJob(t *testing.T) {
	oldStop, oldKill := stopGrace, killGrace
	t.Cleanup(func() { stopGrace, killGrace = oldStop, oldKill })
	stopGrace, killGrace = 0, 0
	w := (&Browser{profile: `C:\synthetic`}).prepareClose(false).(*windowsCloseWitness)
	if w.firstErr == nil || w.firstErr.Error() != "browser_completion_timeout" || w.job != 0 || len(w.set.targets) != 0 {
		t.Fatalf("exhausted preparation began acquisition: %v", w.firstErr)
	}
	if err := w.stop(nil); err == nil {
		t.Fatal("expired witness silently succeeded")
	}
}

func TestCompletionDuplicateJobFailureRetainsOriginalOwnership(t *testing.T) {
	old := completionDuplicateHandle
	t.Cleanup(func() { completionDuplicateHandle = old })
	cause := errors.New("private duplicate operation")
	completionDuplicateHandle = func(_, source, _ windows.Handle, _ *windows.Handle, _ uint32, inherited bool, flags uint32) error {
		if source != 7 || inherited || flags != windows.DUPLICATE_SAME_ACCESS {
			t.Fatal("duplicate must preserve private original job semantics")
		}
		return cause
	}
	g := &group{job: 7}
	duplicate, err := g.duplicateJob()
	if duplicate != 0 || err == nil || err.Error() != "browser_completion_job_duplicate_failed" || !errors.Is(err, cause) || g.job != 7 {
		t.Fatalf("failed duplicate changed ownership or lost cause: duplicate=%d job=%d err=%v", duplicate, g.job, err)
	}
}
