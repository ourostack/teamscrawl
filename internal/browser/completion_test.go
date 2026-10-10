package browser

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type completionFixture struct {
	signalled                bool
	polls, kills, releases   int
	pollErr, killErr, endErr error
}

func (f *completionFixture) target(pid uint32, started int64, inJob bool) *completionTarget {
	return &completionTarget{
		identity: completionIdentity{pid: pid, started: started}, inJob: inJob,
		poll:      func() (bool, error) { f.polls++; return f.signalled, f.pollErr },
		terminate: func() error { f.kills++; return f.killErr },
		release:   func() error { f.releases++; return f.endErr },
	}
}

func completionTestSet(limit int) *completionSet {
	return &completionSet{deadline: time.Now().Add(time.Second), limit: limit}
}

func TestCompletionPendingGenerationBlocksSuccess(t *testing.T) {
	f := &completionFixture{}
	s := completionTestSet(1)
	if err := s.add(f.target(42, 7, false)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.release() }()
	if done, err := s.poll(); done || err != nil {
		t.Fatalf("zero membership cannot replace the held pending signal: %v, %v", done, err)
	}
	f.signalled = true
	if done, err := s.poll(); !done || err != nil {
		t.Fatalf("signalled generation: %v, %v", done, err)
	}
}

func TestCompletionWaitUsesOneDeadline(t *testing.T) {
	polls := 0
	if err := waitCompletion(time.Now().Add(time.Second), func() (bool, error) {
		polls++
		return polls == 2, nil
	}); err != nil || polls != 2 {
		t.Fatalf("later signal: polls %d, %v", polls, err)
	}
	start := time.Now()
	if err := waitCompletion(start.Add(10*time.Millisecond), func() (bool, error) { return false, nil }); err == nil {
		t.Fatal("pending at deadline must fail")
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("poll cadence must not add an allowance after the deadline")
	}
}

func TestCompletionWaitPreservesFailure(t *testing.T) {
	cause := errors.New("private native failure")
	err := waitCompletion(time.Now().Add(time.Second), func() (bool, error) { return false, cause })
	if !errors.Is(err, cause) || strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("fixed printable error and internal cause required: %v", err)
	}
}

func TestCompletionAdmissionBoundsAndDeduplicates(t *testing.T) {
	s := completionTestSet(4096)
	for pid := uint32(1); pid <= 4096; pid++ {
		if err := s.add((&completionFixture{}).target(pid, 1, true)); err != nil {
			t.Fatal(err)
		}
	}
	extra := &completionFixture{}
	if err := s.add(extra.target(4097, 1, true)); err == nil || extra.releases != 1 || len(s.targets) != 4096 {
		t.Fatalf("overflow must refuse and dispose only incoming target: %v, %d", err, extra.releases)
	}
	duplicate := &completionFixture{}
	if err := s.add(duplicate.target(1, 1, true)); err != nil || duplicate.releases != 1 {
		t.Fatalf("duplicate at cap must dispose incoming handle: %v", err)
	}
	if err := s.release(); err != nil {
		t.Fatal(err)
	}
	if len(s.targets) != 0 {
		t.Fatal("released targets still retained")
	}
}

func TestCompletionReusedPIDIsNotDuplicate(t *testing.T) {
	s := completionTestSet(2)
	for _, start := range []int64{7, 8} {
		if err := s.add((&completionFixture{}).target(42, start, false)); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.targets) != 2 {
		t.Fatal("two generations of one PID must retain separate witnesses")
	}
	_ = s.release()
}

func TestCompletionFallbackPreservesFirstErrorAndOwnership(t *testing.T) {
	cause := errors.New("private termination failure")
	bad := &completionFixture{killErr: cause}
	live := &completionFixture{}
	ended := &completionFixture{signalled: true}
	job := &completionFixture{}
	s := completionTestSet(4)
	for i, f := range []*completionFixture{bad, live, ended, job} {
		if err := s.add(f.target(uint32(i+1), 1, f == job)); err != nil { //nolint:gosec // bounded four-entry fixture
			t.Fatal(err)
		}
	}
	if err := s.terminateFallback(); !errors.Is(err, cause) || strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("first fixed failure: %v", err)
	}
	if bad.kills != 1 || live.kills != 1 || ended.kills != 0 || job.kills != 0 {
		t.Fatalf("wrong termination scope: %d %d %d %d", bad.kills, live.kills, ended.kills, job.kills)
	}
	if err := s.release(); err != nil {
		t.Fatal(err)
	}
	_ = s.release()
	for _, f := range []*completionFixture{bad, live, ended, job} {
		if f.releases != 1 {
			t.Fatal("each admitted target must be released exactly once")
		}
	}
}

func TestCompletionFallbackStopsNewWorkAtDeadline(t *testing.T) {
	s := completionTestSet(2)
	first := (&completionFixture{}).target(1, 1, false)
	first.terminate = func() error { s.deadline = time.Now().Add(-time.Second); return nil }
	later := &completionFixture{}
	if err := s.add(first); err != nil {
		t.Fatal(err)
	}
	if err := s.add(later.target(2, 1, false)); err != nil {
		t.Fatal(err)
	}
	if err := s.terminateFallback(); err == nil || later.polls != 0 || later.kills != 0 {
		t.Fatalf("expired budget must not begin the next target: %v", err)
	}
	_ = s.release()
	if later.releases != 1 {
		t.Fatal("deadline cannot suppress owned handle disposal")
	}
}

func TestCompletionEmptyInvalidAndFailedRelease(t *testing.T) {
	s := completionTestSet(1)
	if done, err := s.poll(); !done || err != nil {
		t.Fatalf("empty admitted set: %v %v", done, err)
	}
	if err := s.add(nil); err == nil {
		t.Fatal("invalid target must refuse")
	}
	bad := &completionFixture{}
	if err := s.add(bad.target(0, 0, false)); err == nil || bad.releases != 1 {
		t.Fatal("invalid identity must release incoming handle")
	}
	cause := errors.New("private close failure")
	f := &completionFixture{endErr: cause}
	if err := s.add(f.target(42, 1, false)); err != nil {
		t.Fatal(err)
	}
	if err := s.release(); !errors.Is(err, cause) || strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("handle close failure: %v", err)
	}
	_ = s.release()
	if f.releases != 1 {
		t.Fatal("failed handle close must not be retried implicitly")
	}
}

func TestCompletionKnownWaitFailureIsNotAbsence(t *testing.T) {
	cause := errors.New("private wait failure")
	f := &completionFixture{pollErr: cause}
	s := completionTestSet(1)
	if err := s.add(f.target(42, 1, true)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.release() }()
	if done, err := s.poll(); done || !errors.Is(err, cause) || strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("unknown native wait must not count as absence: %v, %v", done, err)
	}
}

func TestCompletionAdmissionRejectsExpiredAndInvalidOperations(t *testing.T) {
	s := completionTestSet(2)
	f := &completionFixture{}
	invalid := f.target(42, 1, false)
	invalid.release = nil
	if err := s.add(invalid); err == nil {
		t.Fatal("missing disposal operation must refuse")
	}
	s.deadline = time.Now().Add(-time.Second)
	if err := s.add(f.target(42, 1, false)); err == nil || f.releases != 1 {
		t.Fatal("expired admission must dispose the incoming handle")
	}
	s = completionTestSet(2)
	held := &completionFixture{}
	if err := s.add(held.target(42, 1, false)); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("private duplicate release failure")
	duplicate := &completionFixture{endErr: cause}
	if err := s.add(duplicate.target(42, 1, true)); !errors.Is(err, cause) {
		t.Fatalf("duplicate release cause: %v", err)
	}
	if !s.targets[0].inJob {
		t.Fatal("verified private-job membership must preserve job termination authority")
	}
	s.deadline = time.Now().Add(-time.Second)
	if done, err := s.poll(); done || err == nil {
		t.Fatal("expired target polling must refuse")
	}
	_ = s.release()
}

func TestCompletionFallbackContinuesAfterWaitFailureWithinBudget(t *testing.T) {
	cause := errors.New("private wait failure")
	bad := &completionFixture{pollErr: cause}
	good := &completionFixture{}
	s := completionTestSet(2)
	if err := s.add(bad.target(1, 1, false)); err != nil {
		t.Fatal(err)
	}
	if err := s.add(good.target(2, 1, false)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.release() }()
	if err := s.terminateFallback(); !errors.Is(err, cause) || bad.kills != 0 || good.kills != 1 {
		t.Fatalf("first wait failure, no unverified kill, safe peer cleanup: %v", err)
	}
}

func TestCompletionFallbackDeadlineDuringPollPreventsTermination(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "poll-exhausts", true: "first-error-retained"}[failFirst], func(t *testing.T) {
			s := completionTestSet(2)
			cause := errors.New("private first failure")
			f := &completionFixture{}
			target := f.target(1, 1, false)
			if failFirst {
				target.terminate = func() error {
					s.deadline = time.Now().Add(-time.Second)
					return cause
				}
			} else {
				target.poll = func() (bool, error) {
					s.deadline = time.Now().Add(-time.Second)
					return false, nil
				}
			}
			if err := s.add(target); err != nil {
				t.Fatal(err)
			}
			later := &completionFixture{}
			if err := s.add(later.target(2, 1, false)); err != nil {
				t.Fatal(err)
			}
			err := s.terminateFallback()
			if err == nil || later.polls != 0 || later.kills != 0 || f.kills != 0 {
				t.Fatalf("no new native operation after exhaustion: %v", err)
			}
			if failFirst && !errors.Is(err, cause) {
				t.Fatal("timeout must not erase the first native error")
			}
			_ = s.release()
		})
	}
}
