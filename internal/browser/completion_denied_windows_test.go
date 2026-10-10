//go:build windows

package browser

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestCompletionProductionDeniedTerminationWaitsForExactHeldSignal(t *testing.T) {
	f := newCompletionNativeFixture(t)
	f.w.set.deadline = time.Now().Add(time.Minute)
	f.snapshotPids = []uint32{42}
	f.owned[42] = true
	if err := discoverCompletionTargets(f.w); err != nil {
		t.Fatal(err)
	}
	f.afterKill = func() { f.snapshotPids = nil }
	completionTerminateProcess = func(h windows.Handle, _ uint32) error {
		if h != 42 {
			t.Fatal("denied termination targeted an unadmitted generation")
		}
		f.fallbackKills++
		return windows.ERROR_ACCESS_DENIED
	}
	f.onPoll = func(pid uint32) {
		if f.polls[pid] == 6 {
			f.signalled[pid] = true
		}
	}
	if err := f.w.stop(nil); err != nil {
		t.Fatalf("exact held generation completed inside original budget: %v", err)
	}
	if !f.signalled[42] || f.polls[42] < 6 || f.fallbackKills != 1 || f.opened[42] != 3 ||
		f.closed[42] != 3 || f.closed[7] != 1 || f.closed[99] != 4 {
		t.Fatalf("completion or acquisition disposal not observed: polls=%v opened=%v closed=%v", f.polls, f.opened, f.closed)
	}
}

func TestCompletionProductionDeniedTerminationPreservesPendingFailureAndPeerCleanup(t *testing.T) {
	f := newCompletionNativeFixture(t)
	f.w.set.deadline = time.Now().Add(time.Minute)
	f.snapshotPids = []uint32{42, 43}
	f.owned[42], f.owned[43] = true, true
	if err := discoverCompletionTargets(f.w); err != nil {
		t.Fatal(err)
	}
	f.afterKill = func() { f.snapshotPids = nil }
	completionTerminateProcess = func(h windows.Handle, _ uint32) error {
		f.fallbackKills++
		if h == 42 {
			return windows.ERROR_ACCESS_DENIED
		}
		f.signalled[43] = true
		return nil
	}
	f.onPoll = func(pid uint32) {
		if pid == 42 && f.fallbackKills == 2 {
			f.w.set.deadline = time.Now().Add(-time.Second)
		}
	}
	err := f.w.stop(nil)
	if err == nil || err.Error() != "browser_completion_terminate_failed" || !errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		f.fallbackKills != 2 || !f.signalled[43] || f.signalled[42] {
		t.Fatalf("unresolved denial starved peer or lost native cause: %v kills=%d", err, f.fallbackKills)
	}
	if f.closed[42] != f.opened[42] || f.closed[43] != f.opened[43] || f.closed[7] != 1 {
		t.Fatalf("failed completion leaked admitted/transient handles: opened=%v closed=%v", f.opened, f.closed)
	}
}

func TestCompletionNativeDeniedCombinedFailureCannotBecomeProvisional(t *testing.T) {
	for _, stage := range []string{"release", "post-denial-poll"} {
		t.Run(stage, func(t *testing.T) {
			f := newCompletionNativeFixture(t)
			cause := errors.New("synthetic irreversible observation error")
			completionTerminateProcess = func(windows.Handle, uint32) error { return windows.ERROR_ACCESS_DENIED }
			if stage == "release" {
				completionCloseHandle = func(h windows.Handle) error { f.closed[h]++; return cause }
			} else {
				completionWaitProcess = func(h windows.Handle, _ uint32) (uint32, error) {
					f.polls[42]++
					if f.polls[42] == 3 {
						return windows.WAIT_FAILED, cause
					}
					return uint32(windows.WAIT_TIMEOUT), nil
				}
			}
			err := terminateCompletionTarget(42, 42, 142, time.Now().Add(time.Minute))
			if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) || !errors.Is(err, cause) {
				t.Fatalf("combined failure lost original native cause: %v", err)
			}
			if _, provisional := err.(*completionDeniedFailure); provisional || f.closed[42] != 1 {
				t.Fatalf("independent failure became clearable or leaked transient: %v closed=%v", provisional, f.closed)
			}
		})
	}
}
