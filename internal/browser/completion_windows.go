//go:build windows

package browser

import (
	"errors"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	completionWindowsNow   = time.Now
	completionSnapshot     = windows.CreateToolhelp32Snapshot
	completionProcessFirst = windows.Process32First
	completionProcessNext  = windows.Process32Next
	completionTerminateJob = windows.TerminateJobObject
)

type windowsCloseWitness struct {
	set      *completionSet
	job      windows.Handle
	profile  string
	firstErr error
}

func (b *Browser) prepareClose(polite bool) closeWitness {
	budget := stopGrace + 2*killGrace
	if polite {
		budget += closeWait
	}
	w := &windowsCloseWitness{set: &completionSet{deadline: completionWindowsNow().Add(budget), limit: completionMaxTargets}}
	w.profile = completionLongPath(b.profile)
	if err := completionWithin(w.set.deadline); err != nil {
		w.firstErr = err
		return w
	}
	w.job, w.firstErr = b.group.duplicateJob()
	w.remember(discoverCompletionTargets(w))
	return w
}

func (w *windowsCloseWitness) deadline() time.Time { return w.set.deadline }

func (w *windowsCloseWitness) remember(err error) {
	w.firstErr = completionFirst(w.firstErr, err)
}

func (w *windowsCloseWitness) stop(*Browser) (err error) {
	defer func() {
		w.remember(w.set.release())
		if w.job != 0 {
			if closeErr := completionCloseHandle(w.job); closeErr != nil {
				w.remember(&completionFailure{code: "browser_completion_release_failed", cause: closeErr})
			}
			w.job = 0
		}
		err = w.firstErr
	}()
	w.remember(discoverCompletionTargets(w))
	if !completionWindowsNow().Before(w.set.deadline) {
		w.remember(&completionFailure{code: "browser_completion_timeout"})
		return w.firstErr
	}
	if w.job != 0 {
		if killErr := completionTerminateJob(w.job, 1); killErr != nil {
			w.remember(&completionFailure{code: "browser_completion_job_terminate_failed", cause: killErr})
		}
	}
	w.remember(discoverCompletionTargets(w))
	_ = w.set.terminateFallback()
	waitErr := waitCompletion(w.set.deadline, func() (bool, error) {
		done, pollErr := w.set.poll()
		if pollErr != nil {
			return false, pollErr
		}
		if w.job == 0 {
			return false, &completionFailure{code: "browser_completion_job_unavailable"}
		}
		if !completionWindowsNow().Before(w.set.deadline) {
			return false, &completionFailure{code: "browser_completion_timeout"}
		}
		var info jobBasicAccounting
		if queryErr := completionQueryJob(w.job, jobObjectBasicAccountingInformation,
			unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), nil); queryErr != nil { //nolint:gosec // G103: fixed native accounting layout lives through the synchronous query
			return false, &completionFailure{code: "browser_completion_job_query_failed", cause: queryErr}
		}
		return done && info.ActiveProcesses == 0, nil
	})
	w.remember(w.set.terminationFailure(true))
	w.remember(waitErr)
	if waitErr != nil {
		return w.firstErr
	}
	w.remember(discoverCompletionTargets(w))
	done, pollErr := w.set.poll()
	w.remember(pollErr)
	if !done && pollErr == nil {
		w.remember(&completionFailure{code: "browser_completion_late_process"})
		w.remember(w.set.terminateFallback())
		w.remember(w.set.terminationFailure(true))
	}
	return w.firstErr
}

func discoverCompletionTargets(w *windowsCloseWitness) (err error) {
	if !completionWindowsNow().Before(w.set.deadline) {
		return &completionFailure{code: "browser_completion_timeout"}
	}
	if w.job != 0 {
		pids, queryErr := jobPIDs(w.job, completionMaxTargets)
		if queryErr != nil {
			err = queryErr
		} else {
			for _, pid := range pids {
				if !completionWindowsNow().Before(w.set.deadline) {
					return completionFirst(err, &completionFailure{code: "browser_completion_timeout"})
				}
				target, openErr := openCompletionTarget(pid, w.job, w.profile, true, w.set.deadline)
				if openErr == nil && target != nil {
					openErr = w.set.add(target)
				}
				err = completionFirst(err, openErr)
			}
		}
	}
	if !completionWindowsNow().Before(w.set.deadline) {
		return completionFirst(err, &completionFailure{code: "browser_completion_timeout"})
	}
	snap, snapErr := completionSnapshot(windows.TH32CS_SNAPPROCESS, 0)
	if snapErr != nil {
		return completionFirst(err, &completionFailure{code: "browser_completion_snapshot_failed", cause: snapErr})
	}
	defer func() {
		if closeErr := completionCloseHandle(snap); closeErr != nil && err == nil {
			err = &completionFailure{code: "browser_completion_release_failed", cause: closeErr}
		}
	}()
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if late := completionWithin(w.set.deadline); late != nil {
		return completionFirst(err, late)
	}
	nextErr := completionProcessFirst(snap, &entry)
	for scanned := 0; nextErr == nil; scanned++ {
		if scanned >= completionMaxScan {
			return completionFirst(err, &completionFailure{code: "browser_completion_too_large"})
		}
		if !completionWindowsNow().Before(w.set.deadline) {
			return completionFirst(err, &completionFailure{code: "browser_completion_timeout"})
		}
		if entry.ProcessID != uint32(os.Getpid()) { //nolint:gosec // Windows native process ID fits DWORD
			target, openErr := openCompletionTarget(entry.ProcessID, w.job, w.profile, false, w.set.deadline)
			if openErr == nil && target != nil {
				openErr = w.set.add(target)
			}
			err = completionFirst(err, openErr)
		}
		if late := completionWithin(w.set.deadline); late != nil {
			return completionFirst(err, late)
		}
		nextErr = completionProcessNext(snap, &entry)
	}
	if !errors.Is(nextErr, windows.ERROR_NO_MORE_FILES) {
		return completionFirst(err, &completionFailure{code: "browser_completion_snapshot_failed", cause: nextErr})
	}
	return completionFirst(err, completionWithin(w.set.deadline))
}
