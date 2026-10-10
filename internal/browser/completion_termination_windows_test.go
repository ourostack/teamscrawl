//go:build windows

package browser

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestCompletionNativeTerminationOutcomes(t *testing.T) {
	for _, stage := range []string{"success", "already-ended", "open-failed", "creation-failed", "kill-failed", "ended-after-denied", "poll-expired", "open-expired"} {
		t.Run(stage, func(t *testing.T) {
			f := newCompletionNativeFixture(t)
			f.w.set.deadline = time.Now().Add(20 * time.Millisecond)
			deadline := f.w.set.deadline
			kills, created := 0, 0
			completionProcessStart = func(windows.Handle) (int64, error) {
				created++
				if stage == "creation-failed" {
					return 0, errors.New("private creation query")
				}
				return 142, nil
			}
			if stage == "already-ended" {
				f.signalled[42] = true
			}
			completionOpenProcess = func(uint32, bool, uint32) (windows.Handle, error) {
				if stage == "open-failed" {
					return 0, errors.New("private terminate open")
				}
				if stage == "open-expired" {
					time.Sleep(time.Until(deadline) + time.Millisecond)
				}
				return 42, nil
			}
			f.onPoll = func(uint32) {
				if stage == "poll-expired" {
					time.Sleep(time.Until(deadline) + time.Millisecond)
				}
			}
			completionTerminateProcess = func(windows.Handle, uint32) error {
				kills++
				if stage == "ended-after-denied" {
					f.signalled[42] = true
					return windows.ERROR_ACCESS_DENIED
				}
				if stage == "kill-failed" {
					return errors.New("private terminate")
				}
				return nil
			}
			err := terminateCompletionTarget(42, 42, 142, deadline)
			wantSuccess := stage == "success" || stage == "already-ended" || stage == "ended-after-denied"
			if (err == nil) != wantSuccess {
				t.Fatalf("termination outcome %s: %v", stage, err)
			}
			if (stage == "poll-expired" || stage == "open-expired" || stage == "open-failed" || stage == "already-ended") && (kills != 0 || created != 0) {
				t.Fatalf("new identity/kill operation after refusal: kills=%d created=%d", kills, created)
			}
			if stage != "already-ended" && stage != "open-failed" && stage != "poll-expired" && f.closed[42] != 1 {
				t.Fatalf("transient handle must close exactly once: %v", f.closed)
			}
		})
	}
}
