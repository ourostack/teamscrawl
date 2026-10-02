package teamsdesktop

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/ourostack/teamscrawl/internal/errs"
)

func codeOf(t *testing.T, err error) *errs.Coded {
	t.Helper()
	var c *errs.Coded
	if !errors.As(err, &c) {
		t.Fatalf("want *errs.Coded, got %T %v", err, err)
	}
	return c
}

func TestDefaultRoot(t *testing.T) {
	r := DefaultRoot()
	switch goruntime.GOOS {
	case "windows":
		if !strings.Contains(r, filepath.Join("AppData", "Local", "Packages", "MSTeams_8wekyb3d8bbwe", "LocalCache", "Microsoft", "MSTeams", "EBWebView")) {
			t.Fatalf("DefaultRoot = %q", r)
		}
	default:
		if !strings.HasSuffix(r, filepath.Join("Library", "Containers", "com.microsoft.teams2", "Data", "Library", "Application Support", "Microsoft", "MSTeams", "EBWebView")) {
			t.Fatalf("DefaultRoot = %q", r)
		}
	}
	if !filepath.IsAbs(r) {
		t.Fatalf("DefaultRoot not absolute: %q", r)
	}
}

func TestDiscoverFixture(t *testing.T) {
	srcs, other, err := Discover(fixtureRootDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || len(other) != 0 {
		t.Fatalf("sources=%v other=%v", srcs, other)
	}
	s := srcs[0]
	if s.Profile != fixtureProfile || s.Origin != fixtureOrigin {
		t.Fatalf("source = %+v", s)
	}
	if s.Key() != fixtureProfile+"|"+fixtureOrigin {
		t.Fatalf("Key = %q", s.Key())
	}
	if filepath.Base(s.LevelDBDir) != fixtureOrigin+".indexeddb.leveldb" || filepath.Base(s.BlobDir) != fixtureOrigin+".indexeddb.blob" {
		t.Fatalf("dirs = %q %q", s.LevelDBDir, s.BlobDir)
	}
	for _, d := range []string{s.LevelDBDir, s.BlobDir} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Fatalf("%s: %v", d, err)
		}
	}
}

func TestDiscoverOtherOrigins(t *testing.T) {
	root := fakeTree(t, "Default", "https_teams.microsoft.com_0", "https_contoso.sharepoint.com_0", "https_a.example.com_0")
	srcs, other, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].Origin != "https_teams.microsoft.com_0" {
		t.Fatalf("sources = %+v", srcs)
	}
	want := []string{"https_a.example.com_0", "https_contoso.sharepoint.com_0"}
	if len(other) != 2 || other[0] != want[0] || other[1] != want[1] {
		t.Fatalf("otherOrigins = %v, want %v", other, want)
	}
}

func TestDiscoverTeamsCloudHost(t *testing.T) {
	root := fakeTree(t, "Default", "https_teams.cloud.microsoft_0")
	srcs, other, err := Discover(root)
	if err != nil || len(srcs) != 1 || len(other) != 0 || srcs[0].Origin != "https_teams.cloud.microsoft_0" {
		t.Fatalf("srcs=%+v other=%v err=%v", srcs, other, err)
	}
}

func TestDiscoverNoTeamsOrigin(t *testing.T) {
	root := fakeTree(t, "Default", "https_contoso.sharepoint.com_0")
	_, _, err := Discover(root)
	if c := codeOf(t, err); c.Code != errs.CodeNoTeamsOrigin || c.Exit != 3 {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoverNoFDA(t *testing.T) {
	skipIfPermissionDeniedSimulationUnsupported(t)
	root := fakeTree(t, "Default", "https_teams.microsoft.com_0")
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) }) //nolint:gosec // restoring a test temp dir so cleanup can remove it
	_, _, err := Discover(root)
	c := codeOf(t, err)
	if c.Code != errs.CodeNoFullDiskAccess || c.Exit != 3 || !strings.Contains(c.Fix, "Full Disk Access") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoverNoFDAOnInnerDir(t *testing.T) {
	skipIfPermissionDeniedSimulationUnsupported(t)
	root := fakeTree(t, "Default", "https_teams.microsoft.com_0")
	idb := filepath.Join(root, "Default", "IndexedDB")
	if err := os.Chmod(idb, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(idb, 0o700) }) //nolint:gosec // restoring a test temp dir so cleanup can remove it
	_, _, err := Discover(root)
	if c := codeOf(t, err); c.Code != errs.CodeNoFullDiskAccess {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoverMissing(t *testing.T) {
	_, _, err := Discover(filepath.Join(t.TempDir(), "nope"))
	if c := codeOf(t, err); c.Code != errs.CodeTeamsNotInstalled || c.Exit != 3 {
		t.Fatalf("err = %v", err)
	}
}

func TestReadDirMapsOtherErrorsToInternal(t *testing.T) {
	root := t.TempDir()
	oldStatDir, oldReadDirEntries := statDir, readDirEntries
	statDir = func(string) (fs.FileInfo, error) { return os.Stat(root) }
	readDirEntries = func(string) ([]fs.DirEntry, error) { return nil, errors.New("disk on fire") }
	t.Cleanup(func() {
		statDir = oldStatDir
		readDirEntries = oldReadDirEntries
	})

	_, err := readDir(root, root)
	if c := codeOf(t, err); c.Code != errs.CodeInternal || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("err = %v", err)
	}
}
