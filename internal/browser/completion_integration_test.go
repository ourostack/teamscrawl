package browser

import (
	"errors"
	"testing"
	"time"
)

type fixtureCloseWitness struct {
	set *completionSet
}

func (w fixtureCloseWitness) deadline() time.Time { return w.set.deadline }

func (w fixtureCloseWitness) stop(b *Browser) error {
	defer func() { _ = w.set.release() }()
	if b.worthSignallingGroup() {
		stopGroup(b.group)
	}
	if err := sweepArgv(b.profile, true); err != nil {
		return err
	}
	return waitCompletion(w.set.deadline, w.set.poll)
}

func TestCompletionCloseUsesPreCloseHeldWitness(t *testing.T) {
	old := prepareBrowserClose
	t.Cleanup(func() { prepareBrowserClose = old })
	prepared, polls, released := 0, 0, 0
	prepareBrowserClose = func(b *Browser, polite bool) closeWitness {
		if !polite {
			t.Fatal("Close must acquire before its polite phase")
		}
		prepared++
		s := completionTestSet(1)
		s.deadline = time.Now().Add(closeWait + stopGrace + 2*killGrace)
		target := &completionTarget{
			identity:  completionIdentity{pid: 42, started: 7},
			poll:      func() (bool, error) { polls++; return polls >= 2, nil },
			terminate: func() error { return nil },
			release:   func() error { released++; return nil },
		}
		if err := s.add(target); err != nil {
			t.Fatal(err)
		}
		return fixtureCloseWitness{set: s}
	}
	b, _, err := launchFake(t, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if prepared != 1 || polls < 2 || released != 1 {
		t.Fatalf("legacy membership-only Close bypassed held signal: prepared=%d polls=%d released=%d", prepared, polls, released)
	}
}

func TestCompletionCloseCachesFailedWitnessAndRelinquishesLock(t *testing.T) {
	old := prepareBrowserClose
	t.Cleanup(func() { prepareBrowserClose = old })
	prepared, released := 0, 0
	prepareBrowserClose = func(*Browser, bool) closeWitness {
		prepared++
		s := completionTestSet(1)
		f := &completionFixture{}
		target := f.target(42, 7, false)
		target.release = func() error { released++; return nil }
		if err := s.add(target); err != nil {
			t.Fatal(err)
		}
		s.deadline = time.Now()
		return fixtureCloseWitness{set: s}
	}
	b, profile, err := launchFake(t, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, second := b.Close(), b.Close()
	if first == nil || !errors.Is(second, first) || prepared != 1 || released != 1 {
		t.Fatalf("cached Close error and exactly-once disposal: %v %v %d %d", first, second, prepared, released)
	}
	requireLockFree(t, profile)
}
