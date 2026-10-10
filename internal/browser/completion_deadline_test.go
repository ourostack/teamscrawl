package browser

import (
	"testing"
	"time"
)

func TestCompletionLastPollCannotCertifyLateSuccess(t *testing.T) {
	s := completionTestSet(1)
	target := (&completionFixture{}).target(42, 7, false)
	target.poll = func() (bool, error) {
		s.deadline = time.Now().Add(-time.Second)
		return true, nil
	}
	if err := s.add(target); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.release() }()
	if done, err := s.poll(); done || err == nil {
		t.Fatalf("late native result cannot certify completion: %v, %v", done, err)
	}
}

func TestCompletionWaitCannotCertifyLateSuccess(t *testing.T) {
	deadline := time.Now().Add(5 * time.Millisecond)
	err := waitCompletion(deadline, func() (bool, error) {
		time.Sleep(time.Until(deadline) + time.Millisecond)
		return true, nil
	})
	if err == nil {
		t.Fatal("late successful callback must still fail the shared deadline")
	}
}
