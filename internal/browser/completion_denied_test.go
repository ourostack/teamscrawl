package browser

import (
	"errors"
	"testing"
	"time"
)

func TestCompletionDeniedTargetResolvesOnlyAfterFundedHeldSignal(t *testing.T) {
	cause := errors.New("synthetic operation denial")
	signalled, kills, releases := false, 0, 0
	target := &completionTarget{
		identity: completionIdentity{pid: 42, started: 142},
		poll:     func() (bool, error) { return signalled, nil },
		terminate: func() error {
			kills++
			return &completionDeniedFailure{cause: cause}
		},
		release: func() error { releases++; return nil },
	}
	s := completionTestSet(2)
	if err := s.add(target); err != nil {
		t.Fatal(err)
	}
	if err := s.terminateFallback(); err != nil {
		t.Fatalf("provisional denial must not prematurely become irreversible: %v", err)
	}
	if err := s.terminationFailure(true); !errors.Is(err, cause) || err.Error() != "browser_completion_terminate_failed" {
		t.Fatal("pending denial lost its original cause")
	}
	if (&completionDeniedFailure{cause: cause}).Error() != "browser_completion_terminate_failed" {
		t.Fatal("pending native outcome must remain a fixed public failure")
	}
	if done, err := s.poll(); done || err != nil || s.terminationFailure(true) == nil {
		t.Fatalf("pending held generation was declared completed: %v %v", done, err)
	}
	signalled = true
	if done, err := s.poll(); !done || err != nil || s.terminationFailure(true) != nil {
		t.Fatalf("observed exact completion did not resolve denial: %v %v", done, err)
	}
	if err := s.release(); err != nil || kills != 1 || releases != 1 {
		t.Fatalf("termination/disposal changed: %v kills=%d releases=%d", err, kills, releases)
	}
}

func TestCompletionDeniedTargetDoesNotStarveFundedPeer(t *testing.T) {
	cause := errors.New("synthetic denial")
	s := completionTestSet(2)
	signalled, killed, released := false, 0, 0
	for _, pid := range []uint32{42, 43} {
		pid := pid
		target := &completionTarget{
			identity: completionIdentity{pid: pid, started: int64(pid) + 100},
			poll: func() (bool, error) {
				return pid == 43 && signalled, nil
			},
			terminate: func() error {
				if pid == 42 {
					return &completionDeniedFailure{cause: cause}
				}
				killed++
				signalled = true
				return nil
			},
			release: func() error { released++; return nil },
		}
		if err := s.add(target); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.terminateFallback(); err != nil || killed != 1 {
		t.Fatalf("denied target starved funded peer: %v kills=%d", err, killed)
	}
	s.deadline = time.Now().Add(-time.Second)
	if done, err := s.poll(); done || err == nil {
		t.Fatal("unresolved denial became completion after exhaustion")
	}
	if err := s.terminationFailure(true); !errors.Is(err, cause) {
		t.Fatal("unresolved denial was not retained")
	}
	if err := s.release(); err != nil || released != 2 {
		t.Fatalf("partial outcome must release both held targets: %v releases=%d", err, released)
	}
}

func TestCompletionDeniedResolutionPreservesIndependentFailures(t *testing.T) {
	denied, real := errors.New("synthetic denial"), errors.New("synthetic real failure")
	s := completionTestSet(2)
	signalled := false
	for _, pid := range []uint32{42, 43} {
		pid := pid
		if err := s.add(&completionTarget{
			identity: completionIdentity{pid: pid, started: int64(pid) + 100},
			poll:     func() (bool, error) { return signalled, nil },
			terminate: func() error {
				if pid == 42 {
					return &completionDeniedFailure{cause: denied}
				}
				return real
			},
			release: func() error { return nil },
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.terminateFallback(); !errors.Is(err, real) || errors.Is(err, denied) {
		t.Fatalf("irreversible aggregate must distinguish pending denial: %v", err)
	}
	if err := s.terminationFailure(true); !errors.Is(err, denied) || !errors.Is(err, real) {
		t.Fatal("ordered aggregate dropped an unresolved cause")
	}
	signalled = true
	if done, err := s.poll(); !done || err != nil {
		t.Fatalf("held signals: %v %v", done, err)
	}
	if err := s.terminationFailure(true); !errors.Is(err, real) || errors.Is(err, denied) {
		t.Fatal("resolving denial erased a different irreversible error")
	}
	if err := s.terminateFallback(); !errors.Is(err, real) {
		t.Fatal("repeat fallback overwrote prior irreversible error")
	}
	_ = s.release()
}

func TestCompletionDeniedLateOrFailedPollCannotResolve(t *testing.T) {
	for _, failed := range []bool{false, true} {
		s := completionTestSet(1)
		cause, pollErr := errors.New("synthetic denial"), errors.New("synthetic poll failure")
		afterTermination := false
		if err := s.add(&completionTarget{
			identity: completionIdentity{pid: 42, started: 142},
			poll: func() (bool, error) {
				if afterTermination {
					if failed {
						return false, pollErr
					}

					s.deadline = time.Now().Add(-time.Second)
					return true, nil
				}
				return false, nil
			},
			terminate: func() error {
				afterTermination = true
				return &completionDeniedFailure{cause: cause}
			},
			release: func() error { return nil },
		}); err != nil {
			t.Fatal(err)
		}
		_ = s.terminateFallback()
		if done, err := s.poll(); done || err == nil || !errors.Is(s.terminationFailure(true), cause) {
			t.Fatalf("late/failed held signal cleared denial: done=%v err=%v", done, err)
		}
		_ = s.release()
	}
}

func TestCompletionEmptyExpiredSetCannotCertifyCompletion(t *testing.T) {
	s := completionTestSet(1)
	s.deadline = time.Now().Add(-time.Second)
	if done, err := s.poll(); done || err == nil || err.Error() != "browser_completion_timeout" {
		t.Fatalf("empty set after exhaustion must not certify a barrier: done=%v err=%v", done, err)
	}
}

func TestCompletionDeniedPollFailureRemainsIrreversibleAfterLaterSignal(t *testing.T) {
	s := completionTestSet(1)
	denial, failedPoll := errors.New("synthetic denial"), errors.New("synthetic held failure")
	phase := 0
	if err := s.add(&completionTarget{
		identity: completionIdentity{pid: 42, started: 142},
		poll: func() (bool, error) {
			switch phase {
			case 1:
				return false, failedPoll
			case 2:
				return true, nil
			default:
				return false, nil
			}
		},
		terminate: func() error { return &completionDeniedFailure{cause: denial} },
		release:   func() error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.terminateFallback()
	phase = 1
	if done, err := s.poll(); done || !errors.Is(err, failedPoll) {
		t.Fatalf("failed held observation was hidden: %v %v", done, err)
	}
	phase = 2
	if done, err := s.poll(); !done || err != nil {
		t.Fatalf("later held signal should still be observed: %v %v", done, err)
	}
	if err := s.terminationFailure(true); err == nil || err.Error() != "browser_completion_terminate_failed" ||
		!errors.Is(err, denial) || !errors.Is(err, failedPoll) {
		t.Fatalf("later signal erased irreversible combined outcome: %v", err)
	}
	_ = s.release()
}
