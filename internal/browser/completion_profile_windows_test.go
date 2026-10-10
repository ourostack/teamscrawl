//go:build windows

package browser

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestCompletionProductionProfileAdmissionRetainsDisappearedPendingNonleader(t *testing.T) {
	f := newCompletionNativeFixture(t)
	f.snapshotPids = []uint32{42}
	f.owned[42] = true
	if err := discoverCompletionTargets(f.w); err != nil {
		t.Fatal(err)
	}
	if len(f.w.set.targets) != 1 || f.w.set.targets[0].inJob {
		t.Fatal("fixture must exercise verified profile ownership with false private-job membership")
	}
	f.afterKill = func() { f.snapshotPids = nil }
	completionTerminateProcess = func(h windows.Handle, _ uint32) error {
		if h != 42 {
			t.Fatal("fallback must signal only the verified profile generation")
		}
		f.fallbackKills++
		return nil // asynchronous termination leaves the held generation pending
	}
	sawPendingAfterKill := false
	f.onPoll = func(pid uint32) {
		if f.fallbackKills > 0 && f.polls[pid] < 5 {
			sawPendingAfterKill = true
		}
		if f.polls[pid] >= 5 {
			f.signalled[pid] = true
		}
	}
	if err := f.w.stop(nil); err != nil {
		t.Fatal(err)
	}
	if !sawPendingAfterKill || !f.signalled[42] || f.fallbackKills != 1 || f.opened[42] != 3 || f.closed[42] != 3 || f.closed[99] != 4 || f.closed[7] != 1 {
		t.Fatalf("profile-only pending generation must outlive census and complete before exact disposal: polls=%v opened=%v closed=%v", f.polls, f.opened, f.closed)
	}
}
