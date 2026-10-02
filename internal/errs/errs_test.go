package errs

import (
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestConstructorsMatchOutputContract(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		err  *Coded
		code string
		exit int
	}{
		{SnapshotInconsistent("x"), "snapshot_inconsistent", 1},
		{UnsupportedBlockCompression("x"), "unsupported_block_compression", 1},
		{StoreMissing("x"), "store_missing", 1},
		{DBError(cause), "db_error", 1},
		{Internal(cause), "internal", 1},
		{Interrupted(), "interrupted", 1},
		{Usage("x"), "usage", 2},
		{TeamsNotInstalled("/r"), "teams_not_installed", 3},
		{NoFullDiskAccess("/r", cause), "no_full_disk_access", 3},
		{NoTeamsOrigin("/r"), "no_teams_origin", 3},
		{DoctorFailed("x"), "doctor_failed", 3},
		{PartialSync("x"), "partial_sync", 1},
		{Locked("x"), "locked", 4},
	}
	for _, c := range cases {
		if c.err.Code != c.code || c.err.Exit != c.exit {
			t.Errorf("%s: got code=%q exit=%d", c.code, c.err.Code, c.err.Exit)
		}
		if c.err.Message == "" || c.err.Fix == "" {
			t.Errorf("%s: empty message or fix", c.code)
		}
		var e error = c.err
		if e.Error() == "" {
			t.Errorf("%s: empty Error()", c.code)
		}
	}
	var coded *Coded
	if !errors.As(fmt.Errorf("wrap: %w", DBError(cause)), &coded) || !errors.Is(coded, cause) {
		t.Fatal("Coded must be matchable by errors.As and unwrap to its cause")
	}
	fda := NoFullDiskAccess("/r", cause)
	switch goruntime.GOOS {
	case "windows":
		if containsAll(fda.Fix, "System Settings", "Privacy & Security", "Full Disk Access") {
			t.Fatalf("fix = %q", fda.Fix)
		}
	default:
		if !containsAll(fda.Fix, "System Settings", "Privacy & Security", "Full Disk Access") {
			t.Fatalf("fix = %q", fda.Fix)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, x := range subs {
		if !contains(s, x) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestInternalWithNilCauseSaysUnknown(t *testing.T) {
	err := Internal(nil)
	if err.Message != "internal error: unknown error" {
		t.Errorf("message = %q", err.Message)
	}
	if got := Internal(errors.New("boom")).Message; got != "internal error: boom" {
		t.Errorf("message = %q", got)
	}
}

func TestArchiveNewer(t *testing.T) {
	e := ArchiveNewer(5, 2)
	if e.Code != "archive_newer" || e.Exit != ExitEnvironment || !strings.Contains(e.Message, "version 5") || !strings.Contains(e.Message, "version 2") || !strings.Contains(e.Fix, "brew upgrade ourostack/tap/teamscrawl") {
		t.Errorf("%+v", e)
	}
}
