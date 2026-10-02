package teamsdesktop

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fingerprintSource(t *testing.T) Source {
	t.Helper()
	base := t.TempDir()
	s := Source{Profile: "p", Origin: "o", LevelDBDir: filepath.Join(base, "leveldb"), BlobDir: filepath.Join(base, "blob")}
	for p, body := range map[string]string{
		filepath.Join(s.LevelDBDir, "CURRENT"):    "MANIFEST-000001\n",
		filepath.Join(s.LevelDBDir, "000003.log"): "log",
		filepath.Join(s.LevelDBDir, "LOCK"):       "",
		filepath.Join(s.LevelDBDir, "LOG"):        "text log",
		filepath.Join(s.LevelDBDir, "LOG.old"):    "older",
		filepath.Join(s.BlobDir, "1", "00", "2"):  "blobdata",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestFingerprintStable(t *testing.T) {
	s := fingerprintSource(t)
	a, err := FingerprintOf(s)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FingerprintOf(s)
	if err != nil || a != b || len(a) != 64 {
		t.Fatalf("a=%q b=%q err=%v", a, b, err)
	}
	// The committed fixture is also stable.
	f1, err := FingerprintOf(fixtureSource(t))
	if err != nil {
		t.Fatal(err)
	}
	f2, _ := FingerprintOf(fixtureSource(t))
	if f1 != f2 {
		t.Fatal("fixture fingerprint unstable")
	}
}

func TestFingerprintChangesOnWrite(t *testing.T) {
	s := fingerprintSource(t)
	before, _ := FingerprintOf(s)

	log := filepath.Join(s.LevelDBDir, "000003.log")
	if err := os.WriteFile(log, []byte("longer log"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterSize, _ := FingerprintOf(s)
	if afterSize == before {
		t.Fatal("size change not detected")
	}

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(log, later, later); err != nil {
		t.Fatal(err)
	}
	afterTime, _ := FingerprintOf(s)
	if afterTime == afterSize {
		t.Fatal("mtime change not detected")
	}

	blob := filepath.Join(s.BlobDir, "1", "00", "9")
	if err := os.WriteFile(blob, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterBlob, _ := FingerprintOf(s)
	if afterBlob == afterTime {
		t.Fatal("new blob not detected")
	}
}

func TestFingerprintIgnoresLockAndLog(t *testing.T) {
	s := fingerprintSource(t)
	before, _ := FingerprintOf(s)
	for _, n := range []string{"LOCK", "LOG", "LOG.old"} {
		p := filepath.Join(s.LevelDBDir, n)
		if err := os.WriteFile(p, []byte("changed content that is longer"), 0o600); err != nil {
			t.Fatal(err)
		}
		later := time.Now().Add(2 * time.Hour)
		_ = os.Chtimes(p, later, later)
	}
	after, _ := FingerprintOf(s)
	if before != after {
		t.Fatal("LOCK/LOG* changes must not affect the fingerprint")
	}
	// A 000003.log write-ahead log is data, not the text LOG, and must count.
	if err := os.WriteFile(filepath.Join(s.LevelDBDir, "000003.log"), []byte("a much longer wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, _ := FingerprintOf(s); again == before {
		t.Fatal("write-ahead log change ignored")
	}
}

func TestFingerprintMissingBlobDir(t *testing.T) {
	s := fingerprintSource(t)
	_ = os.RemoveAll(s.BlobDir)
	if _, err := FingerprintOf(s); err != nil {
		t.Fatalf("absent blob dir should be fine: %v", err)
	}
}

func TestFingerprintSkipsVanishedFile(t *testing.T) {
	s := fingerprintSource(t)
	victim := filepath.Join(s.LevelDBDir, "000003.log")
	old := statEntry
	statEntry = func(e fs.DirEntry) (fs.FileInfo, error) {
		if e.Name() == "000003.log" {
			_ = os.Remove(victim) // Teams compacts the file between the listing and the stat
			return nil, &fs.PathError{Op: "stat", Path: victim, Err: fs.ErrNotExist}
		}
		return e.Info()
	}
	t.Cleanup(func() { statEntry = old })
	got, err := FingerprintOf(s)
	if err != nil {
		t.Fatalf("a vanished file must be skipped, got %v", err)
	}
	statEntry = old
	if want, _ := FingerprintOf(s); got != want {
		t.Fatal("fingerprint with the file vanished mid-walk must equal the fingerprint without it")
	}
}

func TestFingerprintDependsOnDecoderVersion(t *testing.T) {
	s := fingerprintSource(t)
	before, _ := FingerprintOf(s)
	old := decoderVersion
	decoderVersion = old + 1
	t.Cleanup(func() { decoderVersion = old })
	after, _ := FingerprintOf(s)
	if before == after {
		t.Fatal("bumping DecoderVersion must change the fingerprint of unchanged files")
	}
	if DecoderVersion != old {
		t.Fatal("decoderVersion must start at DecoderVersion")
	}
}

func TestFingerprintSkipsVanishedSubdirectory(t *testing.T) {
	s := fingerprintSource(t)
	want, _ := FingerprintOf(s)
	old := walkDir
	walkDir = func(root string, fn fs.WalkDirFunc) error {
		err := filepath.WalkDir(root, fn)
		if err == nil && root == s.BlobDir { // a subdirectory purged between its listing and its read
			err = fn(filepath.Join(root, "1", "00"), nil, &fs.PathError{Op: "open", Path: filepath.Join(root, "1", "00"), Err: fs.ErrNotExist})
		}
		return err
	}
	t.Cleanup(func() { walkDir = old })
	got, err := FingerprintOf(s)
	if err != nil || got != want {
		t.Fatalf("a vanished subdirectory must be skipped: %v", err)
	}
}
