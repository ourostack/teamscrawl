//go:build windows

package browser

import (
	"errors"
	"testing"
	"time"

	"github.com/ourostack/m365crawl/internal/browser/browsertest"
	"golang.org/x/sys/windows"
)

func TestCompletionCloseNonleaderSignalledAtReturn(t *testing.T) {
	b, profile, err := launchFake(t, map[string]string{browsertest.EnvIgnoreClose: "1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pid := fakePids(t, profile)["child"]
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // Windows PID returned by the owned synthetic child
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	status, err := windows.WaitForSingleObject(h, 0)
	if err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("successful Close must already signal the held nonleader, not merely pass a later census: %d, %v", status, err)
	}
}

func TestCompletionNativeUnknownVersusUnrelated(t *testing.T) {
	oldOpen, oldClose := completionOpenProcess, completionCloseHandle
	oldTime, oldMember, oldArgs := completionProcessStart, completionMembership, completionReadArgs
	t.Cleanup(func() {
		completionOpenProcess, completionCloseHandle = oldOpen, oldClose
		completionProcessStart, completionMembership, completionReadArgs = oldTime, oldMember, oldArgs
	})
	cause := errors.New("private process query")
	deadline := time.Now().Add(time.Second)
	completionOpenProcess = func(uint32, bool, uint32) (windows.Handle, error) { return 0, cause }
	if target, err := openCompletionTarget(42, 7, `C:\profile`, false, deadline); target != nil || err != nil {
		t.Fatal("unclassifiable unrelated host entry should not fail every close")
	}
	if target, err := openCompletionTarget(42, 7, `C:\profile`, true, deadline); target != nil || !errors.Is(err, cause) {
		t.Fatal("known-owned open failure cannot become completed absence")
	}
	completionOpenProcess = func(uint32, bool, uint32) (windows.Handle, error) { return 42, nil }
	closed := 0
	completionCloseHandle = func(windows.Handle) error { closed++; return nil }
	completionProcessStart = func(windows.Handle) (int64, error) { return 7, nil }
	completionMembership = func(_ windows.Handle, job windows.Handle) (bool, error) {
		if job == 0 {
			t.Fatal("NULL job means any job, not ownership by our unavailable private job")
		}
		return false, nil
	}
	completionReadArgs = func(windows.Handle, uint32, time.Time) ([]string, error) {
		return []string{"--user-data-dir=C:\\elsewhere"}, nil
	}
	if target, err := openCompletionTarget(42, 7, `C:\profile`, false, deadline); target != nil || err != nil || closed != 1 {
		t.Fatal("positively unrelated held object must be disposed without admission")
	}
	completionReadArgs = func(windows.Handle, uint32, time.Time) ([]string, error) {
		return []string{"--user-data-dir=C:\\profile"}, nil
	}
	target, err := openCompletionTarget(42, 0, `C:\profile`, false, deadline)
	if err != nil || target == nil || target.inJob {
		t.Fatalf("verified fallback must not infer any-job ownership: %v", err)
	}
	_ = target.release()
	completionProcessStart = func(windows.Handle) (int64, error) { return 0, cause }
	if target, err := openCompletionTarget(42, 7, `C:\profile`, false, deadline); target != nil || !errors.Is(err, cause) {
		t.Fatal("profile ownership plus unknown creation identity must refuse")
	}
	completionReadArgs = func(windows.Handle, uint32, time.Time) ([]string, error) { return nil, cause }
	if target, err := openCompletionTarget(42, 7, `C:\profile`, true, deadline); target != nil || !errors.Is(err, cause) {
		t.Fatal("known-owned unreadable identity must refuse")
	}
}
