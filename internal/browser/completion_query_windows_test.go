//go:build windows

package browser

import (
	"errors"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCompletionNativeArgvQueriesRespectErrorsAndDeadline(t *testing.T) {
	old := completionQueryProcess
	t.Cleanup(func() { completionQueryProcess = old })
	for _, stage := range []string{"short-sizing", "query-failed", "sizing-expired", "second-query-expired"} {
		t.Run(stage, func(t *testing.T) {
			deadline := time.Now().Add(5 * time.Millisecond)
			calls := 0
			cause := errors.New("private native query")
			completionQueryProcess = func(_ windows.Handle, _ int32, ptr unsafe.Pointer, _ uint32, n *uint32) error {
				calls++
				*n = 32
				if ptr == nil {
					if stage == "short-sizing" {
						*n = 0
					}
					if stage == "sizing-expired" {
						time.Sleep(time.Until(deadline) + time.Millisecond)
					}
					return nil
				}
				if stage == "query-failed" {
					return cause
				}
				if stage == "second-query-expired" {
					time.Sleep(time.Until(deadline) + time.Millisecond)
				}
				header := (*windows.NTUnicodeString)(ptr)
				header.Buffer = (*uint16)(unsafe.Add(ptr, 16))
				return nil
			}
			_, err := readProcArgsHandle(42, 65536, deadline)
			if err == nil || (stage == "sizing-expired" && calls != 1) || (stage == "query-failed" && !errors.Is(err, cause)) {
				t.Fatalf("bounded query refusal: stage=%s calls=%d err=%v", stage, calls, err)
			}
		})
	}
}
