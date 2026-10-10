package browser

import (
	"errors"
	"testing"
	"time"

	"github.com/ourostack/m365crawl/internal/browser/browsertest"
	"github.com/ourostack/m365crawl/internal/errs"
)

type failingLaunchWitness struct {
	inner closeWitness
	done  *bool
}

func (w failingLaunchWitness) deadline() time.Time { return w.inner.deadline() }

func (w failingLaunchWitness) stop(b *Browser) error {
	err := w.inner.stop(b)
	*w.done = true
	return errors.Join(err, &completionFailure{code: "browser_completion_controlled_failure"})
}

func TestCompletionLaunchFailureUsesNonPoliteWitness(t *testing.T) {
	old := prepareBrowserClose
	t.Cleanup(func() { prepareBrowserClose = old })
	calls, disposed := 0, false
	prepareBrowserClose = func(b *Browser, polite bool) closeWitness {
		calls++
		if polite {
			t.Fatal("launch failure must not fund a polite CDP allowance")
		}
		return failingLaunchWitness{inner: old(b, polite), done: &disposed}
	}
	b, profile, err := launchFake(t, map[string]string{browsertest.EnvNoPort: "1"}, func(o *LaunchOptions) { o.Timeout = 30 * time.Millisecond })
	var coded *errs.Coded
	if b != nil || !errors.As(err, &coded) || coded.Code != "transcripts_browser_failed" || calls != 1 || !disposed {
		t.Fatalf("original launch failure and exact stop disposition: %v %v %d %v", b, err, calls, disposed)
	}
	requireLockFree(t, profile)
	requireNoProfileProcs(t, profile)
	registry.Lock()
	left := len(registry.live)
	registry.Unlock()
	if left != 0 {
		t.Fatal("post-adoption launch failure remained registered")
	}
}
