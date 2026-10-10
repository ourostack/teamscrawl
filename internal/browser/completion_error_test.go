package browser

import (
	"errors"
	"strings"
	"testing"
)

func TestCompletionPrimaryFixedCodeRetainsLaterCause(t *testing.T) {
	a, b := errors.New("private primary"), errors.New("private later")
	first := &completionFailure{code: "browser_completion_job_query_failed", cause: a}
	next := &completionFailure{code: "browser_completion_snapshot_failed", cause: b}
	got := completionFirst(first, next)
	if got.Error() != "browser_completion_job_query_failed" || !errors.Is(got, a) || !errors.Is(got, b) || strings.Contains(got.Error(), "private") {
		t.Fatalf("primary public operation code and both private causes: %v", got)
	}
	if completionFirst(nil, next) != next || completionFirst(first, nil) != first { //nolint:errorlint // identity is the no-wrap helper contract
		t.Fatal("single error identity changed")
	}
}
