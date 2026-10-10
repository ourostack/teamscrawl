//go:build windows

package browser

import (
	"errors"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type completionNativeFixture struct {
	w             *windowsCloseWitness
	jobPids       []uint32
	snapshotPids  []uint32
	owned         map[uint32]bool
	signalled     map[uint32]bool
	closed        map[windows.Handle]int
	opened        map[windows.Handle]int
	polls         map[uint32]int
	jobKills      int
	fallbackKills int
	snapshots     int
	listPosition  int
	afterKill     func()
	onPoll        func(uint32)
	onSnapshot    func()
	onCreation    func(uint32)
	onEnumeration func()
	queryErr      error
	snapshotErr   error
	openFail      uint32
}

func newCompletionNativeFixture(t *testing.T) *completionNativeFixture {
	t.Helper()
	oldOpen, oldClose, oldStart := completionOpenProcess, completionCloseHandle, completionProcessStart
	oldMember, oldArgs, oldWait := completionMembership, completionReadArgs, completionWaitProcess
	oldQuery, oldSnapshot := completionQueryJob, completionSnapshot
	oldFirst, oldNext, oldKill, oldJobKill := completionProcessFirst, completionProcessNext, completionTerminateProcess, completionTerminateJob
	t.Cleanup(func() {
		completionOpenProcess, completionCloseHandle, completionProcessStart = oldOpen, oldClose, oldStart
		completionMembership, completionReadArgs, completionWaitProcess = oldMember, oldArgs, oldWait
		completionQueryJob, completionSnapshot = oldQuery, oldSnapshot
		completionProcessFirst, completionProcessNext = oldFirst, oldNext
		completionTerminateProcess, completionTerminateJob = oldKill, oldJobKill
	})
	f := &completionNativeFixture{
		w:     &windowsCloseWitness{set: completionTestSet(4096), job: 7, profile: `C:\synthetic`},
		owned: map[uint32]bool{}, signalled: map[uint32]bool{}, closed: map[windows.Handle]int{}, opened: map[windows.Handle]int{}, polls: map[uint32]int{},
	}
	completionOpenProcess = func(_ uint32, _ bool, pid uint32) (windows.Handle, error) {
		if pid == f.openFail {
			return 0, errors.New("private owned open error")
		}
		f.opened[windows.Handle(pid)]++
		return windows.Handle(pid), nil
	}
	completionCloseHandle = func(h windows.Handle) error { f.closed[h]++; return nil }
	completionProcessStart = func(h windows.Handle) (int64, error) {
		if f.onCreation != nil {
			f.onCreation(uint32(h)) //nolint:gosec // fixture handle is the bounded uint32 PID
		}
		return int64(h) + 100, nil //nolint:gosec // fixture handle is the bounded uint32 PID
	}
	completionMembership = func(h, _ windows.Handle) (bool, error) {
		for _, pid := range f.jobPids {
			if h == windows.Handle(pid) {
				return true, nil
			}
		}
		return false, nil
	}
	completionReadArgs = func(h windows.Handle, _ uint32, _ time.Time) ([]string, error) {
		if f.owned[uint32(h)] { //nolint:gosec // fixture handles are bounded uint32 PIDs
			return []string{`--user-data-dir=C:\synthetic`}, nil
		}
		return []string{`--user-data-dir=C:\unrelated`}, nil
	}
	completionWaitProcess = func(h windows.Handle, timeout uint32) (uint32, error) {
		if timeout != 0 {
			t.Fatal("native polls must not allocate per-handle waits")
		}
		pid := uint32(h) //nolint:gosec // fixture handle is the bounded uint32 PID
		f.polls[pid]++
		if f.onPoll != nil {
			f.onPoll(pid)
		}
		if f.signalled[pid] {
			return windows.WAIT_OBJECT_0, nil
		}
		return uint32(windows.WAIT_TIMEOUT), nil
	}
	completionQueryJob = func(_ windows.Handle, class int32, ptr unsafe.Pointer, _ uint32, _ *uint32) error {
		if f.queryErr != nil {
			return f.queryErr
		}
		if class == 3 {
			info := (*completionJobPIDList)(ptr)
			info.assigned, info.returned = uint32(len(f.jobPids)), uint32(len(f.jobPids)) //nolint:gosec // fixture list always within native cap
			for i, pid := range f.jobPids {
				info.pids[i] = uintptr(pid)
			}
		} else {
			(*jobBasicAccounting)(ptr).ActiveProcesses = 0
		}
		return nil
	}
	completionSnapshot = func(uint32, uint32) (windows.Handle, error) {
		f.snapshots++
		if f.onSnapshot != nil {
			f.onSnapshot()
		}
		return 99, f.snapshotErr
	}
	step := func(entry *windows.ProcessEntry32) error {
		if f.onEnumeration != nil {
			f.onEnumeration()
		}
		if f.listPosition >= len(f.snapshotPids) {
			return windows.ERROR_NO_MORE_FILES
		}
		entry.ProcessID = f.snapshotPids[f.listPosition]
		entry.Threads = 0 // zero threads cannot discard an admitted pending generation
		f.listPosition++
		return nil
	}
	completionProcessFirst = func(_ windows.Handle, entry *windows.ProcessEntry32) error {
		f.listPosition = 0
		return step(entry)
	}
	completionProcessNext = func(_ windows.Handle, entry *windows.ProcessEntry32) error { return step(entry) }
	completionTerminateJob = func(windows.Handle, uint32) error {
		f.jobKills++
		if f.afterKill != nil {
			f.afterKill()
		}
		return nil
	}
	completionTerminateProcess = func(h windows.Handle, _ uint32) error {
		f.fallbackKills++
		f.signalled[uint32(h)] = true //nolint:gosec // fixture handle is bounded uint32 PID
		return nil
	}
	return f
}

func TestCompletionProductionKeepsDisappearedNonleaderSignal(t *testing.T) {
	f := newCompletionNativeFixture(t)
	f.jobPids = []uint32{42}
	f.owned[42] = true
	if err := discoverCompletionTargets(f.w); err != nil {
		t.Fatal(err)
	}
	f.afterKill = func() { f.jobPids, f.snapshotPids = nil, nil }
	f.onPoll = func(pid uint32) {
		if f.polls[pid] == 2 {
			f.signalled[pid] = true
		}
	}
	if err := f.w.stop(nil); err != nil {
		t.Fatal(err)
	}
	if f.polls[42] < 2 || f.jobKills != 1 || f.fallbackKills != 0 || f.closed[42] != 2 || f.closed[7] != 1 {
		t.Fatalf("missing retained completion/duplicate disposal: polls=%v kills=%d/%d closed=%v", f.polls, f.jobKills, f.fallbackKills, f.closed)
	}
}

func TestCompletionProductionRefreshesBeforeFirstJobTermination(t *testing.T) {
	f := newCompletionNativeFixture(t)
	f.jobPids = []uint32{42}
	f.owned[42] = true
	f.afterKill = func() {
		if len(f.w.set.targets) != 1 || f.w.set.targets[0].identity.pid != 42 {
			t.Fatal("native termination preceded refreshed exact-generation admission")
		}
		f.signalled[42] = true
	}
	if err := f.w.stop(nil); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionProductionPartialFailurePreservesPrimaryAndCleansPeers(t *testing.T) {
	f := newCompletionNativeFixture(t)
	f.jobPids, f.snapshotPids = []uint32{42, 43}, []uint32{44}
	f.owned[44], f.openFail = true, 43
	f.afterKill = func() { f.jobPids = nil; f.signalled[42] = true }
	err := f.w.stop(nil)
	if err == nil || err.Error() != "browser_completion_process_open_failed" || f.jobKills != 1 || f.fallbackKills != 1 || f.closed[7] != 1 {
		t.Fatalf("first owned error and safe verified peer cleanup: %v; job/fallback=%d/%d closes=%v", err, f.jobKills, f.fallbackKills, f.closed)
	}
	if f.opened[42] != 1 || f.opened[44] != 4 || f.closed[42] != 1 || f.closed[44] != 4 || f.closed[43] != 0 || f.closed[99] != 3 {
		t.Fatalf("every held/duplicate/transient/snapshot acquisition must dispose once: opened=%v closed=%v", f.opened, f.closed)
	}
}

func TestCompletionDiscoveryFirstCodeSurvivesSnapshotFailure(t *testing.T) {
	f := newCompletionNativeFixture(t)
	first, later := errors.New("private first job error"), errors.New("private snapshot error")
	f.queryErr, f.snapshotErr = first, later
	err := discoverCompletionTargets(f.w)
	if err == nil || err.Error() != "browser_completion_job_query_failed" || !errors.Is(err, first) || !errors.Is(err, later) {
		t.Fatalf("later error replaced the primary fixed code/cause: %v", err)
	}
}

func TestCompletionDiscoveryDeadlineStopsNewNativeTransitions(t *testing.T) {
	for _, stage := range []string{"creation", "snapshot", "last-target"} {
		t.Run(stage, func(t *testing.T) {
			f := newCompletionNativeFixture(t)
			f.w.set.deadline = time.Now().Add(5 * time.Millisecond)
			deadline := f.w.set.deadline
			f.jobPids = []uint32{42}
			f.snapshotPids = []uint32{42}
			lateOps := 0
			oldMember := completionMembership
			completionMembership = func(h, j windows.Handle) (bool, error) {
				if !time.Now().Before(f.w.set.deadline) {
					lateOps++
				}
				return oldMember(h, j)
			}
			switch stage {
			case "creation":
				f.onCreation = func(uint32) { time.Sleep(time.Until(deadline) + time.Millisecond) }
			case "snapshot":
				f.onSnapshot = func() { time.Sleep(time.Until(deadline) + time.Millisecond) }
				f.onEnumeration = func() { lateOps++ }
			default:
				f.jobPids = nil
				f.onCreation = func(uint32) { time.Sleep(time.Until(deadline) + time.Millisecond) }
				f.onEnumeration = func() {
					if !time.Now().Before(f.w.set.deadline) {
						lateOps++
					}
				}
			}
			if err := discoverCompletionTargets(f.w); err == nil || lateOps != 0 {
				t.Fatalf("deadline must prohibit new membership/enumeration work: %v; late=%d", err, lateOps)
			}
		})
	}
}

func TestCompletionProductionLateTargetRefusesWithoutNewWait(t *testing.T) {
	f := newCompletionNativeFixture(t)
	f.onSnapshot = func() {
		if f.snapshots == 3 {
			f.snapshotPids = []uint32{42}
			f.owned[42] = true
		}
	}
	err := f.w.stop(nil)
	if err == nil || err.Error() != "browser_completion_late_process" || f.fallbackKills != 1 || f.polls[42] != 4 {
		t.Fatalf("late target must fail then use only funded safe termination, no drain: %v; kills=%d polls=%v", err, f.fallbackKills, f.polls)
	}
}
