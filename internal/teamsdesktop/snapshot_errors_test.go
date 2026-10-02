package teamsdesktop

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ourostack/teamscrawl/internal/errs"
	"github.com/ourostack/teamscrawl/internal/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

func skipIfRoot(t *testing.T) {
	t.Helper()
	skipIfPermissionDeniedSimulationUnsupported(t)
}

// compactedSource is a source whose LevelDB has one table and a consistent MANIFEST.
func compactedSource(t *testing.T) Source {
	t.Helper()
	s, _ := manifestSource(t)
	return s
}

func tableOf(t *testing.T, dir string) string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.ldb"))
	if err != nil || len(m) == 0 {
		t.Fatalf("no table in %s: %v", dir, err)
	}
	return m[0]
}

func TestSnapshotTempDirFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("TMPDIR", missing)
	t.Setenv("TMP", missing)
	t.Setenv("TEMP", missing)
	snap, cleanup, err := Snapshot(context.Background(), compactedSource(t))
	cleanup()
	var coded *errs.Coded
	if snap != "" || !errors.As(err, &coded) || coded.Code != errs.CodeInternal {
		t.Fatalf("snap=%q err=%v", snap, err)
	}
}

func TestSnapshotSourceMissingIsNotRetried(t *testing.T) {
	privateTempDir(t)
	attempts := 0
	old := onSnapshotAttempt
	onSnapshotAttempt = func(n int) { attempts = n }
	t.Cleanup(func() { onSnapshotAttempt = old })
	s := Source{LevelDBDir: filepath.Join(t.TempDir(), "gone")}
	snap, cleanup, err := Snapshot(context.Background(), s)
	cleanup()
	if snap != "" || err == nil || attempts != 1 {
		t.Fatalf("snap=%q attempts=%d err=%v", snap, attempts, err)
	}
}

func TestSnapshotBlobCopyFailureIsNotRetried(t *testing.T) {
	privateTempDir(t)
	s := compactedSource(t)
	// A blob "directory" that is a file lands on the existing destination directory.
	s.BlobDir = filepath.Join(t.TempDir(), "blobfile")
	if err := os.WriteFile(s.BlobDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, cleanup, err := Snapshot(context.Background(), s)
	cleanup()
	if snap != "" || err == nil {
		t.Fatalf("snap=%q err=%v", snap, err)
	}
}

func TestCopyOnceDestinationErrors(t *testing.T) {
	s := compactedSource(t)
	// The destination is a regular file, so its subdirectories cannot be made.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if retry, err := copyOnce(context.Background(), s, f, 1); retry || err == nil {
		t.Fatalf("file destination: retry=%v err=%v", retry, err)
	}
	// An old copy that cannot be removed.
	skipIfRoot(t)
	dir := t.TempDir()
	stuck := filepath.Join(dir, "leveldb")
	if err := os.MkdirAll(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "old"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o500); err != nil { //nolint:gosec // a read-only directory is the point of the test
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) }) //nolint:gosec // restoring a test temp dir so cleanup can remove it
	if retry, err := copyOnce(context.Background(), s, dir, 1); retry || err == nil {
		t.Fatalf("stuck old copy: retry=%v err=%v", retry, err)
	}
}

// validationOutcome copies s once, with mutate run on the copy before it is validated.
func validationOutcome(t *testing.T, s Source, mutate func(copyDir string)) (bool, error) {
	t.Helper()
	dir := t.TempDir()
	old := afterCopy
	afterCopy = func(int) {
		if mutate != nil {
			mutate(filepath.Join(dir, "leveldb"))
		}
	}
	t.Cleanup(func() { afterCopy = old })
	return copyOnce(context.Background(), s, dir, 1)
}

func TestCopyOnceValidationOutcomes(t *testing.T) {
	// A table the MANIFEST names but the copy lacks: retry.
	s := compactedSource(t)
	retry, err := validationOutcome(t, s, func(c string) { _ = os.Remove(tableOf(t, c)) })
	var missing *leveldb.MissingFileError
	if !retry || !errors.As(err, &missing) {
		t.Fatalf("missing table: retry=%v err=%v", retry, err)
	}

	// A block with an unsupported compression type: a coded error, no retry.
	s = compactedSource(t)
	retry, err = validationOutcome(t, s, func(c string) { setCompressionType(t, tableOf(t, c), 2) })
	if retry || codeOf(t, err).Code != errs.CodeUnsupportedBlockCompression {
		t.Fatalf("unsupported compression: retry=%v err=%v", retry, err)
	}

	// A table that cannot be read: no full disk access.
	skipIfRoot(t)
	s = compactedSource(t)
	retry, err = validationOutcome(t, s, func(c string) {
		if err := os.Chmod(tableOf(t, c), 0); err != nil {
			t.Fatal(err)
		}
	})
	if retry || codeOf(t, err).Code != errs.CodeNoFullDiskAccess {
		t.Fatalf("permission: retry=%v err=%v", retry, err)
	}

	// A damaged table in the live cache: a database error, no retry.
	s = compactedSource(t)
	if err := os.WriteFile(tableOf(t, s.LevelDBDir), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	retry, err = validationOutcome(t, s, nil)
	if retry || codeOf(t, err).Code != errs.CodeDBError {
		t.Fatalf("damaged table: retry=%v err=%v", retry, err)
	}
}

// setCompressionType rewrites the trailer of the table's first data block, with a valid
// checksum, so only the compression type is wrong.
func setCompressionType(t *testing.T, path string, typ byte) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	metaOff, _ := binary.Uvarint(b[len(b)-48:])
	at := int(metaOff) - 5 //nolint:gosec // small test table
	b[at] = typ
	binary.LittleEndian.PutUint32(b[at+1:], util.NewCRC(b[:at+1]).Value())
	if err := os.WriteFile(path, b, 0o600); err != nil { //nolint:gosec // test temp dir
		t.Fatal(err)
	}
}

func TestSameManifestSizeErrors(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if err := sameManifestSize(src, dst, "MANIFEST-000001\n"); err == nil || !strings.Contains(err.Error(), "live cache") {
		t.Fatalf("absent in source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "MANIFEST-000001"), []byte("ab"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sameManifestSize(src, dst, "MANIFEST-000001\n"); err == nil || !strings.Contains(err.Error(), "missing from the copy") {
		t.Fatalf("absent in copy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "MANIFEST-000001"), []byte("ab"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sameManifestSize(src, dst, "MANIFEST-000001\n"); err != nil {
		t.Fatalf("equal sizes: %v", err)
	}
	for _, bad := range []string{"", "  \n", "../x", `a\b`} {
		if err := sameManifestSize(src, dst, bad); err != nil {
			t.Errorf("a bad CURRENT %q is left for the reader to report, got %v", bad, err)
		}
	}
}

func TestCopyLevelDBSkipsDirectoriesAndLogs(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{"CURRENT": "M\n", "MANIFEST-000001": "m", "000002.log": "l", "LOCK": "", "LOG": "t"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(src, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A link to a directory is listed as a non-directory entry but is not a regular file: skipped.
	if err := os.Symlink(src, filepath.Join(src, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := copyLevelDB(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadDir(dst)
	var names []string
	for _, e := range got {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "000002.log,CURRENT,MANIFEST-000001" {
		t.Fatalf("copied %v", names)
	}
}

func TestCopyLevelDBUnreadableFile(t *testing.T) {
	skipIfRoot(t)
	src, dst := t.TempDir(), t.TempDir()
	p := filepath.Join(src, "000002.log")
	if err := os.WriteFile(p, []byte("l"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	if err := copyLevelDB(context.Background(), src, dst); codeOf(t, err).Code != errs.CodeNoFullDiskAccess {
		t.Fatalf("err = %v", err)
	}
}

func TestCopyFileErrors(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(context.Background(), filepath.Join(dir, "absent"), filepath.Join(dir, "d1")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent source: %v", err)
	}
	// The destination already exists: it is never overwritten.
	existing := filepath.Join(dir, "existing")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(context.Background(), src, existing); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing destination: %v", err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep" { //nolint:gosec // test temp dir
		t.Fatal("existing destination overwritten")
	}
	// A directory is not copied and creates nothing.
	d2 := filepath.Join(dir, "d2")
	if err := copyFile(context.Background(), dir, d2); err != nil {
		t.Fatalf("directory source: %v", err)
	}
	if _, err := os.Stat(d2); err == nil {
		t.Fatal("a directory source created a destination")
	}
	// A cancelled context stops the copy.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyFile(ctx, src, filepath.Join(dir, "d3")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

type chunkReader struct {
	chunks [][]byte
	end    error
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, c.end
	}
	n := copy(p, c.chunks[0])
	c.chunks = c.chunks[1:]
	return n, nil
}

func TestCopyStream(t *testing.T) {
	var out bytes.Buffer
	if err := copyStream(context.Background(), &out, &chunkReader{chunks: [][]byte{[]byte("ab"), []byte("cd")}, end: io.EOF}); err != nil || out.String() != "abcd" {
		t.Fatalf("copy = %q, %v", out.String(), err)
	}
	boom := errors.New("disk full")
	if err := copyStream(context.Background(), failWriter{boom}, &chunkReader{chunks: [][]byte{[]byte("x")}, end: io.EOF}); !errors.Is(err, boom) {
		t.Fatalf("write failure: %v", err)
	}
	rboom := errors.New("read failed")
	if err := copyStream(context.Background(), io.Discard, &chunkReader{chunks: [][]byte{[]byte("x")}, end: rboom}); !errors.Is(err, rboom) {
		t.Fatalf("read failure: %v", err)
	}
}

func TestCopyTreeBehaviors(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "1", "00"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "1", "00", "2"), []byte("blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "1", "00", "2"), filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "1", "00", "2")); err != nil || string(b) != "blob" { //nolint:gosec // test temp dir
		t.Fatalf("blob not copied: %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); err == nil {
		t.Fatal("a symlink was copied")
	}
	// A missing blob directory is fine.
	if err := copyTree(context.Background(), filepath.Join(src, "absent"), t.TempDir()); err != nil {
		t.Fatalf("absent blob dir: %v", err)
	}
	// A cancelled context stops the walk with the context error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyTree(ctx, src, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	// A file that cannot be read.
	skipIfRoot(t)
	locked := filepath.Join(src, "1", "00", "2")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(context.Background(), src, t.TempDir()); codeOf(t, err).Code != errs.CodeNoFullDiskAccess {
		t.Fatalf("unreadable blob: %v", err)
	}
}

func TestCopyTreeWalkErrors(t *testing.T) {
	src := t.TempDir()
	old := walkDir
	t.Cleanup(func() { walkDir = old })
	// A directory purged between its listing and its read is skipped.
	walkDir = func(root string, fn fs.WalkDirFunc) error {
		return fn(filepath.Join(root, "gone"), nil, &fs.PathError{Op: "open", Err: fs.ErrNotExist})
	}
	if err := copyTree(context.Background(), src, t.TempDir()); err != nil {
		t.Fatalf("vanished directory: %v", err)
	}
	// Any other walk error is reported.
	walkDir = func(root string, fn fs.WalkDirFunc) error {
		return fn(root, nil, errors.New("walk failed"))
	}
	if err := copyTree(context.Background(), src, t.TempDir()); err == nil {
		t.Fatal("walk failure swallowed")
	}
	// A path outside the root cannot be made relative.
	walkDir = func(root string, fn fs.WalkDirFunc) error {
		return fn("relative/elsewhere", fs.FileInfoToDirEntry(mustInfo(t, src)), nil)
	}
	if err := copyTree(context.Background(), src, t.TempDir()); err == nil {
		t.Fatal("unrelatable path accepted")
	}
	// A subdirectory that cannot be created in the copy.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	walkDir = func(root string, fn fs.WalkDirFunc) error {
		return fn(filepath.Join(root, "sub"), fs.FileInfoToDirEntry(mustInfo(t, src)), nil)
	}
	if err := copyTree(context.Background(), src, file); err == nil {
		t.Fatal("uncreatable subdirectory accepted")
	}
	// A file that vanishes between the listing and the copy is skipped.
	walkDir = func(root string, fn fs.WalkDirFunc) error {
		ghost := filepath.Join(root, "ghost")
		if err := os.WriteFile(ghost, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		e := fs.FileInfoToDirEntry(mustInfo(t, ghost))
		if err := os.Remove(ghost); err != nil {
			t.Fatal(err)
		}
		return fn(ghost, e, nil)
	}
	if err := copyTree(context.Background(), src, t.TempDir()); err != nil {
		t.Fatalf("vanished file: %v", err)
	}
}

func TestMapFSError(t *testing.T) {
	if err := mapFSError("p", context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
	if err := mapFSError("p", context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: %v", err)
	}
	if c := codeOf(t, mapFSError("p", &fs.PathError{Err: fs.ErrPermission})); c.Code != errs.CodeNoFullDiskAccess {
		t.Errorf("permission: %v", c)
	}
	if c := codeOf(t, mapFSError("p", errors.New("other"))); c.Code != errs.CodeInternal {
		t.Errorf("other: %v", c)
	}
}

func TestSweepStaleSnapshotsSkipsFreshAndForeign(t *testing.T) {
	tmp := t.TempDir()
	stale := filepath.Join(tmp, snapshotPrefix+"old")
	fresh := filepath.Join(tmp, snapshotPrefix+"new")
	other := filepath.Join(tmp, "unrelated")
	file := filepath.Join(tmp, snapshotPrefix+"file")
	for _, d := range []string{stale, fresh, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if n := SweepStaleSnapshots(tmp, 24*time.Hour); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	for _, p := range []string{fresh, other, file} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed", p)
		}
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale snapshot kept")
	}
	if n := SweepStaleSnapshots(filepath.Join(tmp, "absent"), time.Hour); n != 0 {
		t.Errorf("absent dir swept %d", n)
	}
}

func TestCopyOnceCannotCreateDestination(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // a read-only directory is the point of the test
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // restoring a test temp dir so cleanup can remove it
	if retry, err := copyOnce(context.Background(), compactedSource(t), dir, 1); retry || err == nil {
		t.Fatalf("read-only destination: retry=%v err=%v", retry, err)
	}
}

func TestCopyFileStatFailure(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("fstat failed")
	old := statOpen
	statOpen = func(*os.File) (fs.FileInfo, error) { return nil, boom }
	t.Cleanup(func() { statOpen = old })
	dst := filepath.Join(dir, "dst")
	if err := copyFile(context.Background(), src, dst); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("a destination was created before the source was known to be a file")
	}
}
