package browser

import "testing"

func TestCompletionNativeBufferExtentIsNotReportedCapacity(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		capacity, returned, header, offset, n uint32
		want                                  bool
	}{
		{"valid", 64, 64, 16, 16, 48, true},
		{"empty", 64, 16, 16, 16, 0, true},
		{"native-size-outgrows-buffer", 64, 128, 16, 16, 0, false},
		{"short-header", 64, 15, 16, 16, 0, false},
		{"inside-header", 64, 64, 16, 8, 0, false},
		{"pointer-after-returned", 64, 32, 16, 40, 0, false},
		{"text-after-returned", 64, 32, 16, 24, 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := completionBufferFits(tc.capacity, tc.returned, tc.header, tc.offset, tc.n); got != tc.want {
				t.Fatalf("native buffer admission=%v, want %v", got, tc.want)
			}
		})
	}
}
