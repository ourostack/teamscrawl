//go:build windows

package browser

import (
	"testing"
	"unsafe"

	"github.com/ourostack/m365crawl/internal/browser/browsertest"
	"golang.org/x/sys/windows"
)

func TestCompletionDuplicateJobQueryAfterConcurrentOriginalRelease(t *testing.T) {
	b, profile, err := launchFake(t, map[string]string{browsertest.EnvIgnoreClose: "1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pid := fakePids(t, profile)["child"]
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // owned synthetic child native PID
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	w := b.prepareClose(true).(*windowsCloseWitness)
	if w.firstErr != nil {
		t.Fatal(w.firstErr)
	}
	released := make(chan struct{})
	go func() {
		KillAll()
		b.group.release()
		close(released)
	}()
	<-released
	var info jobBasicAccounting
	if err := windows.QueryInformationJobObject(w.job, jobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		t.Fatalf("original release invalidated independently owned job: %v", err)
	}
	if err := b.stopWith(w); err != nil {
		t.Fatal(err)
	}
	if state, err := windows.WaitForSingleObject(h, 0); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("completion after force-quit/release: %d %v", state, err)
	}
	// Prevent the fake-launch cleanup from reacquiring the intentionally released job.
	b.closeOnce.Do(func() { b.closeErr = nil })
}
