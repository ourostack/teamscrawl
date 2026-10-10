//go:build windows

package browser

import (
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCompletionNativeJobCountBounds(t *testing.T) {
	old := completionQueryJob
	t.Cleanup(func() { completionQueryJob = old })
	for _, count := range []uint32{4096, 4097} {
		completionQueryJob = func(_ windows.Handle, _ int32, ptr unsafe.Pointer, _ uint32, _ *uint32) error {
			info := (*completionJobPIDList)(ptr)
			info.assigned, info.returned = count, count
			for i := range info.pids {
				info.pids[i] = uintptr(i + 1)
			}
			return nil
		}
		pids, err := jobPIDs(7, 4096)
		if count == 4096 && (err != nil || len(pids) != 4096) {
			t.Fatalf("exact job cap: %d %v", len(pids), err)
		}
		if count == 4097 && err == nil {
			t.Fatal("reported overflow must refuse before indexing")
		}
	}
}

func TestCompletionNativeSnapshotEntryBounds(t *testing.T) {
	for _, count := range []int{65536, 65537} {
		t.Run(map[int]string{65536: "exact", 65537: "overflow"}[count], func(t *testing.T) {
			f := newCompletionNativeFixture(t)
			f.w.set.deadline = time.Now().Add(time.Minute)
			f.snapshotPids = make([]uint32, count)
			for i := range f.snapshotPids {
				f.snapshotPids[i] = 42 // repeated unrelated entries still consume inspection work
			}
			err := discoverCompletionTargets(f.w)
			if count == 65536 && err != nil {
				t.Fatal(err)
			}
			if count == 65537 && (err == nil || err.Error() != "browser_completion_too_large") {
				t.Fatalf("snapshot overflow: %v", err)
			}
			if f.closed[99] != 1 || f.closed[42] != 65536 {
				t.Fatalf("snapshot/temporary target disposal: %v", f.closed)
			}
		})
	}
}

func TestCompletionNativeArgvBufferBoundaries(t *testing.T) {
	old := completionQueryProcess
	t.Cleanup(func() { completionQueryProcess = old })
	for _, tc := range []struct {
		name                   string
		size, returned, offset uint32
		length, maximum        uint16
		wantOK                 bool
	}{
		{"valid-empty", 32, 32, 16, 0, 0, true},
		{"exact-cap", 65536, 65536, 16, 0, 0, true},
		{"sizing-overflow", 65537, 65537, 16, 0, 0, false},
		{"returned-outside-allocation", 32, 64, 16, 0, 0, false},
		{"inside-header", 32, 32, 8, 0, 0, false},
		{"pointer-after-buffer", 32, 32, 40, 0, 0, false},
		{"odd-length", 32, 32, 16, 1, 2, false},
		{"length-over-maximum", 32, 32, 16, 4, 2, false},
		{"text-after-returned", 32, 24, 16, 10, 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			completionQueryProcess = func(_ windows.Handle, _ int32, ptr unsafe.Pointer, _ uint32, n *uint32) error {
				calls++
				if ptr == nil {
					*n = tc.size
					return nil
				}
				header := (*windows.NTUnicodeString)(ptr)
				header.Length, header.MaximumLength = tc.length, tc.maximum
				header.Buffer = (*uint16)(unsafe.Add(ptr, tc.offset))
				*n = tc.returned
				return nil
			}
			_, err := readProcArgsHandle(42, 65536, time.Now().Add(time.Second))
			if (err == nil) != tc.wantOK {
				t.Fatalf("argv query admission: %v", err)
			}
			if tc.size > 65536 && calls != 1 {
				t.Fatal("oversize sizing must refuse before allocation/second native query")
			}
		})
	}
}
