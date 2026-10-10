//go:build windows

package browser

import (
	"errors"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCompletionNativeWaitUsesHeldSignal(t *testing.T) {
	old := completionWaitProcess
	t.Cleanup(func() { completionWaitProcess = old })
	for _, status := range []uint32{windows.WAIT_OBJECT_0, uint32(windows.WAIT_TIMEOUT), windows.WAIT_FAILED} {
		completionWaitProcess = func(h windows.Handle, timeout uint32) (uint32, error) {
			if h != 42 || timeout != 0 {
				t.Fatal("wait must use the retained handle and no per-process timeout")
			}
			return status, nil
		}
		done, err := pollCompletionHandle(42)
		switch status {
		case windows.WAIT_OBJECT_0:
			if !done || err != nil {
				t.Fatalf("signalled: %v, %v", done, err)
			}
		case uint32(windows.WAIT_TIMEOUT):
			if done || err != nil {
				t.Fatalf("pending: %v, %v", done, err)
			}
		default:
			if done || err == nil {
				t.Fatal("unexpected wait status must not certify completion")
			}
		}
	}
	cause := errors.New("private native wait")
	completionWaitProcess = func(windows.Handle, uint32) (uint32, error) { return windows.WAIT_FAILED, cause }
	if _, err := pollCompletionHandle(42); !errors.Is(err, cause) {
		t.Fatal("native wait cause lost")
	}
}

func TestCompletionNativeTerminationRejectsReusedPID(t *testing.T) {
	oldOpen, oldClose := completionOpenProcess, completionCloseHandle
	oldTime, oldWait, oldKill := completionProcessStart, completionWaitProcess, completionTerminateProcess
	t.Cleanup(func() {
		completionOpenProcess, completionCloseHandle = oldOpen, oldClose
		completionProcessStart, completionWaitProcess, completionTerminateProcess = oldTime, oldWait, oldKill
	})
	completionWaitProcess = func(windows.Handle, uint32) (uint32, error) { return uint32(windows.WAIT_TIMEOUT), nil }
	completionOpenProcess = func(rights uint32, inherit bool, pid uint32) (windows.Handle, error) {
		if pid != 42 || inherit || rights&windows.PROCESS_TERMINATE == 0 {
			t.Fatal("termination open must target the observed PID with noninherited termination rights")
		}
		return 99, nil
	}
	completionProcessStart = func(h windows.Handle) (int64, error) {
		if h != 99 {
			t.Fatal("creation identity must be checked on the newly acquired target handle")
		}
		return 8, nil
	}
	closed, killed := 0, 0
	completionCloseHandle = func(h windows.Handle) error {
		if h != 99 {
			t.Fatal("only the new transient handle may close")
		}
		closed++
		return nil
	}
	completionTerminateProcess = func(windows.Handle, uint32) error { killed++; return nil }
	if err := terminateCompletionTarget(7, 42, 7, time.Now().Add(time.Second)); err == nil || killed != 0 || closed != 1 {
		t.Fatalf("replacement must not be signalled; transient handle disposed: %v, %d, %d", err, killed, closed)
	}
}

func TestCompletionNativeJobListRejectsTruncation(t *testing.T) {
	old := completionQueryJob
	t.Cleanup(func() { completionQueryJob = old })
	completionQueryJob = func(_ windows.Handle, class int32, ptr unsafe.Pointer, _ uint32, _ *uint32) error {
		if class != 3 {
			t.Fatal("must query the private job's PID list")
		}
		info := (*completionJobPIDList)(ptr)
		info.assigned, info.returned = 2, 1
		info.pids[0] = 42
		return nil
	}
	if _, err := jobPIDs(7, 4096); err == nil {
		t.Fatal("truncated job membership must refuse without buffer growth")
	}
	completionQueryJob = func(_ windows.Handle, _ int32, ptr unsafe.Pointer, _ uint32, _ *uint32) error {
		info := (*completionJobPIDList)(ptr)
		info.assigned, info.returned = 1, 1
		info.pids[0] = 42
		return nil
	}
	pids, err := jobPIDs(7, 4096)
	if err != nil || len(pids) != 1 || pids[0] != 42 {
		t.Fatalf("complete native job list: %v, %v", pids, err)
	}
}

func TestCompletionDuplicateJobSurvivesRelease(t *testing.T) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := &group{job: job}
	duplicate, err := g.duplicateJob()
	if err != nil {
		g.release()
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(duplicate) }()
	g.release()
	g.release()
	if g.job != 0 || g.alive() {
		t.Fatal("released original job must be inert, not a stale numeric handle")
	}
	g.term()
	g.kill()
	if _, err := jobPIDs(duplicate, 4096); err != nil {
		t.Fatalf("local completion job reference invalidated by original release: %v", err)
	}
	if _, err := g.duplicateJob(); err == nil {
		t.Fatal("cannot silently acquire an unknown released job")
	}
}
