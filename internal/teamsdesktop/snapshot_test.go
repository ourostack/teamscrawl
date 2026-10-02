package teamsdesktop

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ourostack/teamscrawl/internal/errs"
	"github.com/ourostack/teamscrawl/internal/indexeddb"
	"github.com/ourostack/teamscrawl/internal/leveldb"
)

func TestSnapshotFixtureOpens(t *testing.T) {
	snap, cleanup, err := Snapshot(context.Background(), fixtureSource(t))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.HasPrefix(filepath.Base(snap), "teamscrawl-snapshot-") {
		t.Fatalf("snapDir = %q", snap)
	}
	o, err := indexeddb.Open(filepath.Join(snap, "leveldb"), filepath.Join(snap, "blob"))
	if err != nil {
		t.Fatal(err)
	}
	dbs, err := o.Databases()
	if err != nil || len(dbs) != 8 {
		t.Fatalf("dbs=%d err=%v", len(dbs), err)
	}
	// Teams' lock and text log are not copied.
	for _, n := range []string{"LOCK", "LOG", "LOG.old"} {
		if _, err := os.Stat(filepath.Join(snap, "leveldb", n)); err == nil {
			t.Errorf("%s copied", n)
		}
	}
}

func TestSnapshotCleanup(t *testing.T) {
	snap, cleanup, err := Snapshot(context.Background(), fixtureSource(t))
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(snap)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		assertCurrentUserAndSystemOnly(t, snap)
		for _, sub := range []string{"leveldb", "blob"} {
			assertCurrentUserAndSystemOnly(t, filepath.Join(snap, sub))
		}
	} else {
		if st.Mode().Perm() != 0o700 {
			t.Fatalf("snapDir mode = %v", st.Mode().Perm())
		}
		for _, sub := range []string{"leveldb", "blob"} {
			if st, err := os.Stat(filepath.Join(snap, sub)); err != nil || st.Mode().Perm() != 0o700 {
				t.Fatalf("%s: %v %v", sub, st, err)
			}
		}
	}
	cleanup()
	if _, err := os.Stat(snap); !os.IsNotExist(err) {
		t.Fatalf("snapshot still present: %v", err)
	}
	cleanup() // idempotent
}

// privateTempDir points os.TempDir at a directory of this test's own, so counting snapshot
// directories is not disturbed by other packages' tests running at the same time.
func privateTempDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
}

func TestSnapshotRetryThenFail(t *testing.T) {
	privateTempDir(t)
	base := t.TempDir()
	s := Source{Profile: "p", Origin: "o", LevelDBDir: filepath.Join(base, "leveldb"), BlobDir: filepath.Join(base, "blob")}
	tabs := fakeLevelDB(t, s.LevelDBDir)
	if err := os.Remove(tabs[0]); err != nil { // manifest now names a missing table
		t.Fatal(err)
	}
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "teamscrawl-snapshot-*"))

	attempts := 0
	old := onSnapshotAttempt
	onSnapshotAttempt = func(int) { attempts++ }
	t.Cleanup(func() { onSnapshotAttempt = old })

	snap, cleanup, err := Snapshot(context.Background(), s)
	if snap != "" || cleanup == nil {
		t.Fatalf("snap=%q cleanup nil=%v", snap, cleanup == nil)
	}
	cleanup()
	c := codeOf(t, err)
	if c.Code != errs.CodeSnapshotInconsistent || c.Exit != 1 {
		t.Fatalf("err = %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "teamscrawl-snapshot-*"))
	if len(after) != len(before) {
		t.Fatalf("snapshot dirs leaked: before %d after %d", len(before), len(after))
	}
}

func TestSnapshotRetryThenSucceed(t *testing.T) {
	base := t.TempDir()
	s := Source{Profile: "p", Origin: "o", LevelDBDir: filepath.Join(base, "leveldb"), BlobDir: filepath.Join(base, "blob")}
	tabs := fakeLevelDB(t, s.LevelDBDir)
	hidden := tabs[0] + ".hidden"
	if err := os.Rename(tabs[0], hidden); err != nil {
		t.Fatal(err)
	}
	old := onSnapshotAttempt
	onSnapshotAttempt = func(n int) {
		if n == 2 { // Teams finishes its write between attempts
			_ = os.Rename(hidden, tabs[0])
		}
	}
	t.Cleanup(func() { onSnapshotAttempt = old })
	snap, cleanup, err := Snapshot(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	db, err := leveldb.Load(filepath.Join(snap, "leveldb"))
	if err != nil || db.Stats().Keys != 50 {
		t.Fatalf("keys=%v err=%v", db, err)
	}
}

func TestSnapshotCurrentChangedRetries(t *testing.T) {
	base := t.TempDir()
	s := Source{Profile: "p", Origin: "o", LevelDBDir: filepath.Join(base, "leveldb")}
	fakeLevelDB(t, s.LevelDBDir)
	cur := filepath.Join(s.LevelDBDir, "CURRENT")
	orig, _ := os.ReadFile(cur) //nolint:gosec // test temp dir
	attempts := 0
	oldCopy, oldAttempt := afterCopy, onSnapshotAttempt
	afterCopy = func(n int) {
		attempts = n
		if n == 1 { // CURRENT flips mid-copy on the first attempt only
			_ = os.WriteFile(cur, []byte("MANIFEST-999999\n"), 0o600)
		}
	}
	onSnapshotAttempt = func(n int) {
		if n == 2 { // Teams has settled again
			_ = os.WriteFile(cur, orig, 0o600) //nolint:gosec // test temp dir
		}
	}
	t.Cleanup(func() { afterCopy, onSnapshotAttempt = oldCopy, oldAttempt })
	_, cleanup, err := Snapshot(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestSnapshotCancel(t *testing.T) {
	privateTempDir(t)
	// Cancelled before the copy: error, nothing left behind.
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "teamscrawl-snapshot-*"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snap, cleanup, err := Snapshot(ctx, fixtureSource(t))
	if err == nil || snap != "" {
		t.Fatalf("snap=%q err=%v", snap, err)
	}
	cleanup()
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "teamscrawl-snapshot-*"))
	if len(after) != len(before) {
		t.Fatalf("leaked on pre-cancel: %d -> %d", len(before), len(after))
	}

	// Cancelled after a successful snapshot: it removes itself.
	ctx2, cancel2 := context.WithCancel(context.Background())
	snap, cleanup, err = Snapshot(ctx2, fixtureSource(t))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cancel2()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(snap); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot not removed after cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSweepStaleSnapshots(t *testing.T) {
	tmp := t.TempDir()
	old := filepath.Join(tmp, "teamscrawl-snapshot-old")
	fresh := filepath.Join(tmp, "teamscrawl-snapshot-fresh")
	other := filepath.Join(tmp, "unrelated-old")
	for _, d := range []string{old, fresh, other} {
		if err := os.MkdirAll(filepath.Join(d, "leveldb"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ago := time.Now().Add(-48 * time.Hour)
	for _, d := range []string{old, other} {
		_ = os.Chtimes(d, ago, ago)
	}
	if n := SweepStaleSnapshots(tmp, 24*time.Hour); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old snapshot kept")
	}
	for _, d := range []string{fresh, other} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s removed", d)
		}
	}
}

// manifestSource is a fake LevelDB source and the path of its live MANIFEST.
func manifestSource(t *testing.T) (Source, string) {
	t.Helper()
	base := t.TempDir()
	s := Source{Profile: "p", Origin: "o", LevelDBDir: filepath.Join(base, "leveldb")}
	fakeLevelDB(t, s.LevelDBDir)
	cur, err := os.ReadFile(filepath.Join(s.LevelDBDir, "CURRENT")) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	return s, filepath.Join(s.LevelDBDir, strings.TrimSpace(string(cur)))
}

func TestSnapshotManifestSizeMismatchRetries(t *testing.T) {
	s, manifest := manifestSource(t)
	orig, _ := os.ReadFile(manifest) //nolint:gosec // test temp dir
	attempts := 0
	oldCopy, oldAttempt := afterCopy, onSnapshotAttempt
	afterCopy = func(n int) {
		attempts = n
		if n == 1 { // Teams appends to the MANIFEST after we copied it
			f, _ := os.OpenFile(manifest, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test temp dir
			_, _ = f.Write([]byte("more"))
			_ = f.Close()
		}
	}
	onSnapshotAttempt = func(n int) {
		if n == 2 {
			_ = os.WriteFile(manifest, orig, 0o600) //nolint:gosec // test temp dir
		}
	}
	t.Cleanup(func() { afterCopy, onSnapshotAttempt = oldCopy, oldAttempt })
	_, cleanup, err := Snapshot(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestSnapshotManifestSizeMismatchFails(t *testing.T) {
	s, manifest := manifestSource(t)
	attempts := 0
	oldCopy := afterCopy
	afterCopy = func(n int) {
		attempts = n
		f, _ := os.OpenFile(manifest, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test temp dir
		_, _ = f.Write([]byte("x"))
		_ = f.Close()
	}
	t.Cleanup(func() { afterCopy = oldCopy })
	snap, cleanup, err := Snapshot(context.Background(), s)
	cleanup()
	if snap != "" || codeOf(t, err).Code != errs.CodeSnapshotInconsistent || attempts != 3 {
		t.Fatalf("snap=%q attempts=%d err=%v", snap, attempts, err)
	}
	if !strings.Contains(err.Error(), "MANIFEST") {
		t.Fatalf("error should name the MANIFEST: %v", err)
	}
}
