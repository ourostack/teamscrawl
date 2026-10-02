package syncer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ourostack/teamscrawl/internal/errs"
	"github.com/ourostack/teamscrawl/internal/store"
	"github.com/ourostack/teamscrawl/internal/teamsdesktop"
)

const fixtureRoot = "../../testdata/teams-fixture/EBWebView"

var (
	acctA = teamsdesktop.Account{TenantID: "00000000-0000-4000-8000-000000000001", UserID: "00000000-0000-4000-8000-0000000000a1", Locale: "en-us"}
	acctB = teamsdesktop.Account{TenantID: "00000000-0000-4000-8000-000000000002", UserID: "00000000-0000-4000-8000-0000000000a2", Locale: "en-us"}
)

// Golden totals of the fixture (distinct rows in testdata/teams-fixture/expected/mapped-*.json).
const (
	fixtureMessages      = 110
	fixtureConversations = 14
	fixtureActivity      = 22
)

func newDB(t *testing.T) string { return filepath.Join(t.TempDir(), "data", "teamscrawl.db") }

// isolateTmp points snapshots at a private temp dir so leaks are detectable.
func isolateTmp(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	setTempDirEnv(t, tmp)
	return tmp
}

func setTempDirEnv(t *testing.T, tmp string) {
	t.Helper()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
}

func snapshotDirs(t *testing.T, tmp string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(tmp, "teamscrawl-snapshot-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func run(t *testing.T, o Options) (Report, []Change) {
	t.Helper()
	r, ch, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return r, ch
}

func readStatus(t *testing.T, db string) store.StatusRow {
	t.Helper()
	s, err := store.OpenReadOnly(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func totals(st store.StatusRow) (convs, msgs, acts int) {
	for _, a := range st.Accounts {
		convs += a.Conversations
		msgs += a.Messages
		acts += a.Activity
	}
	return
}

// copyTree copies a directory tree, preserving nothing but names and bytes.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if e.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := os.ReadFile(p) //nolint:gosec // test fixture
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600) //nolint:gosec // G703: test copy into a temp dir
	})
	if err != nil {
		t.Fatal(err)
	}
}

func fixtureCopy(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "EBWebView")
	copyTree(t, fixtureRoot, root)
	return root
}

func logFile(t *testing.T, root string) string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(root, "*", "IndexedDB", "*.leveldb", "*.log"))
	if len(m) != 1 {
		t.Fatalf("log files: %v", m)
	}
	return m[0]
}

func TestSyncFixture(t *testing.T) {
	tmp := isolateTmp(t)
	db := newDB(t)
	start := time.Now()
	r, changes := run(t, Options{Root: fixtureRoot, DBPath: db})
	if r.Status != "ok" {
		t.Fatalf("status = %q, omissions %v", r.Status, r.Omissions)
	}
	if r.StartedAt.Before(start.Add(-time.Second)) || r.FinishedAt.Before(r.StartedAt) {
		t.Fatalf("times: %v %v", r.StartedAt, r.FinishedAt)
	}
	if r.Messages.Inserted != fixtureMessages || r.Messages.Seen != fixtureMessages ||
		r.Conversations.Inserted != fixtureConversations || r.Activity.Inserted != fixtureActivity {
		t.Fatalf("counts: msgs %+v convs %+v act %+v", r.Messages, r.Conversations, r.Activity)
	}
	if r.People.Inserted == 0 || r.People.Updated != 0 {
		t.Fatalf("people: %+v", r.People)
	}
	if len(r.Sources) != 1 || r.Sources[0].Status != "ok" || !strings.Contains(r.Sources[0].Source, "https_teams.microsoft.com_0") {
		t.Fatalf("sources: %+v", r.Sources)
	}
	if len(r.Omissions) != 0 {
		t.Fatalf("omissions: %v", r.Omissions)
	}
	st := readStatus(t, db)
	c, m, a := totals(st)
	if c != fixtureConversations || m != fixtureMessages || a != fixtureActivity || len(st.Accounts) != 2 {
		t.Fatalf("archive holds %d convs %d msgs %d activity, %d accounts", c, m, a, len(st.Accounts))
	}
	if st.LastRun == nil || st.LastRun.Status != "ok" {
		t.Fatalf("last run: %+v", st.LastRun)
	}
	// Every inserted message and activity item is reported as a new change.
	kinds := map[string]int{}
	for _, ch := range changes {
		if ch.Change != "new" || ch.Key == "" {
			t.Fatalf("change: %+v", ch)
		}
		kinds[ch.Kind]++
	}
	if kinds["message"] != fixtureMessages || kinds["activity"] != fixtureActivity {
		t.Fatalf("changes by kind: %v", kinds)
	}
	if left := snapshotDirs(t, tmp); len(left) != 0 {
		t.Fatalf("snapshot left behind: %v", left)
	}
	// Report JSON is snake_case.
	b, _ := json.Marshal(r)
	for _, want := range []string{`"status"`, `"sources"`, `"conversations"`, `"messages"`, `"people"`, `"activity"`, `"omissions"`, `"other_origins"`, `"started_at"`, `"finished_at"`, `"inserted"`, `"unchanged"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("report JSON lacks %s: %s", want, b)
		}
	}
}

func TestSecondSyncUnchanged(t *testing.T) {
	db := newDB(t)
	run(t, Options{Root: fixtureRoot, DBPath: db})
	before := readStatus(t, db)
	time.Sleep(5 * time.Millisecond)
	r, changes := run(t, Options{Root: fixtureRoot, DBPath: db})
	if r.Status != "unchanged" || len(r.Sources) != 1 || r.Sources[0].Status != "unchanged" {
		t.Fatalf("report: %+v", r)
	}
	zero := store.Counts{}
	if r.Messages != zero || r.Conversations != zero || r.People != zero || r.Activity != zero || len(changes) != 0 {
		t.Fatalf("an unchanged run wrote: %+v %v", r, changes)
	}
	after := readStatus(t, db)
	if !after.Accounts[0].LastSyncedAt.Equal(before.Accounts[0].LastSyncedAt) {
		t.Fatalf("an unchanged run touched the accounts: %v -> %v", before.Accounts[0].LastSyncedAt, after.Accounts[0].LastSyncedAt)
	}
	if after.LastRun == nil || after.LastRun.Status != "unchanged" || after.LastRun.ID <= before.LastRun.ID {
		t.Fatalf("the attempt should still be recorded: %+v", after.LastRun)
	}
}

func TestSyncAfterWrite(t *testing.T) {
	root := fixtureCopy(t)
	db := newDB(t)
	run(t, Options{Root: root, DBPath: db})
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(logFile(t, root), later, later); err != nil {
		t.Fatal(err)
	}
	r, changes := run(t, Options{Root: root, DBPath: db})
	if r.Status != "ok" {
		t.Fatalf("status = %q", r.Status)
	}
	if r.Messages.Seen != fixtureMessages || r.Messages.Unchanged != fixtureMessages ||
		r.Conversations.Unchanged != fixtureConversations || r.Conversations.Seen != fixtureConversations ||
		r.Activity.Unchanged != fixtureActivity || r.People.Unchanged != r.People.Seen || r.People.Seen == 0 {
		t.Fatalf("a re-decode must change no rows: msgs %+v convs %+v act %+v people %+v", r.Messages, r.Conversations, r.Activity, r.People)
	}
	if len(changes) != 0 {
		t.Fatalf("changes for unchanged rows: %v", changes)
	}
}

// omittingBlob finds the blob file, relative to the origin's blob directory, whose removal makes
// exactly one allowlisted record undecodable.
func omittingBlob(t *testing.T) string {
	t.Helper()
	root := fixtureCopy(t)
	blobDir, _ := filepath.Glob(filepath.Join(root, "*", "IndexedDB", "*.blob"))
	if len(blobDir) != 1 {
		t.Fatalf("blob dirs: %v", blobDir)
	}
	var files []string
	_ = filepath.WalkDir(blobDir[0], func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			rel, _ := filepath.Rel(blobDir[0], p)
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	for _, rel := range files {
		p := filepath.Join(blobDir[0], rel)
		keep, _ := os.ReadFile(p) //nolint:gosec // test fixture copy
		_ = os.Remove(p)
		r, _, err := Run(context.Background(), Options{Root: root, DBPath: newDB(t)})
		if err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(p, keep, 0o600) //nolint:gosec // G703: test fixture copy
		if r.Omissions["blob_missing"] == 1 {
			return rel
		}
	}
	t.Fatal("no fixture blob is needed by an allowlisted record")
	return ""
}

func TestSyncOmissions(t *testing.T) {
	tmp := isolateTmp(t)
	rel := omittingBlob(t)
	root := fixtureCopy(t)
	blobDir, _ := filepath.Glob(filepath.Join(root, "*", "IndexedDB", "*.blob"))
	if err := os.Remove(filepath.Join(blobDir[0], rel)); err != nil {
		t.Fatal(err)
	}
	r, _ := run(t, Options{Root: root, DBPath: newDB(t)})
	if r.Status != "ok_with_omissions" || r.Omissions["blob_missing"] != 1 {
		t.Fatalf("status %q omissions %v", r.Status, r.Omissions)
	}
	if r.Sources[0].Status != "ok_with_omissions" || r.Sources[0].Omissions["blob_missing"] != 1 {
		t.Fatalf("source report: %+v", r.Sources[0])
	}
	if left := snapshotDirs(t, tmp); len(left) != 0 {
		t.Fatalf("snapshot left behind: %v", left)
	}
}

func TestSyncLocked(t *testing.T) {
	db := newDB(t)
	release, err := store.AcquireLock(db)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	r, ch, err := Run(context.Background(), Options{Root: fixtureRoot, DBPath: db})
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.CodeLocked || coded.Exit != errs.ExitLocked {
		t.Fatalf("err = %v", err)
	}
	if r.Status != "" || ch != nil {
		t.Fatalf("a failed run returns no report: %+v", r)
	}
}

func TestSyncAccountFilter(t *testing.T) {
	db := newDB(t)
	r, _ := run(t, Options{Root: fixtureRoot, DBPath: db, Account: &acctA})
	if r.Status != "ok" {
		t.Fatalf("status = %q", r.Status)
	}
	st := readStatus(t, db)
	if len(st.Accounts) != 1 || st.Accounts[0].UserID != acctA.UserID || st.Accounts[0].TenantID != acctA.TenantID {
		t.Fatalf("accounts: %+v", st.Accounts)
	}
	c, m, a := totals(st)
	if c == 0 || m == 0 || a == 0 || m >= fixtureMessages || c >= fixtureConversations {
		t.Fatalf("filtered archive: %d %d %d", c, m, a)
	}
	if r.Messages.Inserted != m || r.Conversations.Inserted != c || r.Activity.Inserted != a {
		t.Fatalf("report %+v vs archive %d %d %d", r, c, m, a)
	}
}

func TestFilteredRunNeverSkipsOtherAccount(t *testing.T) {
	db := newDB(t)
	run(t, Options{Root: fixtureRoot, DBPath: db, Account: &acctA})
	// The same unchanged cache, now for B: must decode, not skip.
	r, _ := run(t, Options{Root: fixtureRoot, DBPath: db, Account: &acctB})
	if r.Status != "ok" || r.Messages.Inserted == 0 {
		t.Fatalf("B's run: %+v", r)
	}
	st := readStatus(t, db)
	c, m, a := totals(st)
	if len(st.Accounts) != 2 || c != fixtureConversations || m != fixtureMessages || a != fixtureActivity {
		t.Fatalf("after A then B: %d accounts, %d %d %d", len(st.Accounts), c, m, a)
	}
	// A filtered run always decodes, even when the cache is unchanged.
	r, _ = run(t, Options{Root: fixtureRoot, DBPath: db, Account: &acctB})
	if r.Status != "ok" || r.Sources[0].Status != "ok" || r.Messages.Unchanged == 0 {
		t.Fatalf("repeat filtered run: %+v", r)
	}
	// Filtered runs store no fingerprint, so an unfiltered run after them does not skip either.
	r, _ = run(t, Options{Root: fixtureRoot, DBPath: db})
	if r.Status != "ok" {
		t.Fatalf("unfiltered run after filtered ones: %q", r.Status)
	}
	// ...and then the unfiltered fingerprint is recorded and skips.
	r, _ = run(t, Options{Root: fixtureRoot, DBPath: db})
	if r.Status != "unchanged" {
		t.Fatalf("second unfiltered run: %q", r.Status)
	}
	// A filtered run after that still decodes.
	r, _ = run(t, Options{Root: fixtureRoot, DBPath: db, Account: &acctA})
	if r.Status != "ok" {
		t.Fatalf("filtered after unchanged: %q", r.Status)
	}
}

func TestDecoderVersionBumpResyncs(t *testing.T) {
	db := newDB(t)
	run(t, Options{Root: fixtureRoot, DBPath: db})
	srcs, _, err := teamsdesktop.Discover(fixtureRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the newest stored fingerprint by one taken under another DecoderVersion: the
	// fingerprint hashes the version, so the same files give a different value.
	s, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.RecordRun(context.Background(), store.Run{StartedAt: now, FinishedAt: now, Source: srcs[0].Key(), Fingerprint: "fingerprint-under-decoder-version-0", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	r, _ := run(t, Options{Root: fixtureRoot, DBPath: db})
	if r.Status != "ok" || r.Messages.Seen != fixtureMessages || r.Messages.Unchanged != fixtureMessages {
		t.Fatalf("a different stored fingerprint must decode again: %+v", r)
	}
}

func TestFailedRunRecorded(t *testing.T) {
	db := newDB(t)
	missing := filepath.Join(t.TempDir(), "no-teams")
	r, ch, err := Run(context.Background(), Options{Root: missing, DBPath: db})
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.CodeTeamsNotInstalled {
		t.Fatalf("err = %v", err)
	}
	if r.Status != "" || ch != nil {
		t.Fatalf("a failed run returns no report: %+v", r)
	}
	st := readStatus(t, db)
	if st.LastRun == nil || st.LastRun.Status != "failed" {
		t.Fatalf("last run: %+v", st.LastRun)
	}
	if st.LastSuccessAt.After(time.Time{}) {
		t.Fatalf("a failed run is not a success: %v", st.LastSuccessAt)
	}
	// A failed run never becomes the fingerprint to skip against.
	if r, _ := run(t, Options{Root: fixtureRoot, DBPath: db}); r.Status != "ok" {
		t.Fatalf("status after a failure: %q", r.Status)
	}
}

func TestPauseHookAndCancel(t *testing.T) {
	tmp := isolateTmp(t)
	db := newDB(t)
	t.Setenv("TEAMSCRAWL_TEST_PAUSE_AFTER_SNAPSHOT", "150ms")
	start := time.Now()
	run(t, Options{Root: fixtureRoot, DBPath: db})
	if time.Since(start) < 150*time.Millisecond {
		t.Fatalf("the pause hook did not pause: %v", time.Since(start))
	}

	// A cancelled run stops during the pause, removes its snapshot and records a failure.
	t.Setenv("TEAMSCRAWL_TEST_PAUSE_AFTER_SNAPSHOT", "30s")
	// Cancel on the observable signal (the pause marker on stderr), not after a wall-clock delay:
	// under load a fixed deadline can expire before the run ever reaches the pause.
	paused := captureStderrMarker(t, testPauseMarker)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-paused
		cancel()
	}()
	_, _, err := Run(ctx, Options{Root: fixtureRoot, DBPath: db, Account: &acctA})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if left := snapshotDirs(t, tmp); len(left) != 0 {
		t.Fatalf("snapshot left behind: %v", left)
	}
	if st := readStatus(t, db); st.LastRun.Status != "failed" {
		t.Fatalf("cancelled run: %+v", st.LastRun)
	}
}

// captureStderrMarker redirects os.Stderr for the test and returns a channel that closes once a
// line containing marker has been written to it.
func captureStderrMarker(t *testing.T, marker string) <-chan struct{} {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	seen := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var once sync.Once
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if strings.Contains(sc.Text(), marker) {
				once.Do(func() { close(seen) })
			}
		}
	}()
	t.Cleanup(func() {
		os.Stderr = old
		_ = w.Close()
		<-done
		_ = r.Close()
	})
	return seen
}

func TestSweepsStaleSnapshots(t *testing.T) {
	tmp := isolateTmp(t)
	stale := filepath.Join(tmp, "teamscrawl-snapshot-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	run(t, Options{Root: fixtureRoot, DBPath: newDB(t)})
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale snapshot kept: %v", err)
	}
}

func TestProgressOutput(t *testing.T) {
	var buf strings.Builder
	run(t, Options{Root: fixtureRoot, DBPath: newDB(t), Progress: &buf})
	if !strings.Contains(buf.String(), "https_teams.microsoft.com_0") {
		t.Fatalf("progress = %q", buf.String())
	}
}

// flushLog records every batch the syncer hands to the store.
type flushLog struct {
	kinds []string
	sizes []int
}

func hookFlush(t *testing.T, size int, fn func(kind string, n int) error) *flushLog {
	t.Helper()
	log := &flushLog{}
	oldSize, oldHook := batchSize, beforeFlush
	batchSize = size
	beforeFlush = func(kind string, n int) error {
		log.kinds, log.sizes = append(log.kinds, kind), append(log.sizes, n)
		if fn != nil {
			return fn(kind, n)
		}
		return nil
	}
	t.Cleanup(func() { batchSize, beforeFlush = oldSize, oldHook })
	return log
}

func TestBatchesAreBounded(t *testing.T) {
	log := hookFlush(t, 7, nil)
	root := fixtureCopy(t)
	db := newDB(t)
	r, changes := run(t, Options{Root: root, DBPath: db})
	if r.Messages.Inserted != fixtureMessages || r.Conversations.Inserted != fixtureConversations || r.Activity.Inserted != fixtureActivity || len(changes) != fixtureMessages+fixtureActivity {
		t.Fatalf("batched counts: %+v %+v %+v %d", r.Messages, r.Conversations, r.Activity, len(changes))
	}
	perKind := map[string]int{}
	for i, n := range log.sizes {
		if n > 7 || n == 0 {
			t.Fatalf("batch of %d records (limit 7)", n)
		}
		perKind[log.kinds[i]] += n
	}
	if perKind["message"] != fixtureMessages || perKind["conversation"] != fixtureConversations || perKind["activity"] != fixtureActivity {
		t.Fatalf("flushed per kind: %v", perKind)
	}
	if len(log.sizes) < fixtureMessages/7 {
		t.Fatalf("only %d batches", len(log.sizes))
	}
	// Re-decoding through batches changes nothing.
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(logFile(t, root), later, later)
	r, _ = run(t, Options{Root: root, DBPath: db})
	if r.Messages.Unchanged != fixtureMessages || r.Conversations.Unchanged != fixtureConversations || r.Activity.Unchanged != fixtureActivity || r.People.Unchanged != r.People.Seen {
		t.Fatalf("second batched run: %+v %+v %+v %+v", r.Messages, r.Conversations, r.Activity, r.People)
	}
}

func TestFailedSourceRollsBackEverything(t *testing.T) {
	sawMessages := false
	log := hookFlush(t, 10, func(kind string, _ int) error {
		if kind == "message" {
			sawMessages = true
		}
		if kind == "activity" {
			return errors.New("injected activity failure")
		}
		return nil
	})
	db := newDB(t)
	r, ch, err := Run(context.Background(), Options{Root: fixtureRoot, DBPath: db})
	if err == nil || r.Status != StatusFailed || ch != nil {
		t.Fatalf("%v %+v %v", err, r, ch)
	}
	if !sawMessages {
		t.Fatalf("messages were not flushed before the activity failure: %v", log.kinds)
	}
	st := readStatus(t, db)
	if c, m, a := totals(st); c+m+a != 0 || len(st.Accounts) != 0 {
		t.Fatalf("a failed source left rows: %d %d %d %+v", c, m, a, st.Accounts)
	}
	if st.LastRun == nil || st.LastRun.Status != "failed" || !st.LastSuccessAt.IsZero() {
		t.Fatalf("last run: %+v", st.LastRun)
	}
	// The next run decodes the source again and reports every row as new.
	beforeFlush = func(string, int) error { return nil }
	r, ch, err = Run(context.Background(), Options{Root: fixtureRoot, DBPath: db})
	if err != nil || r.Status != "ok" || r.Messages.Inserted != fixtureMessages || len(ch) != fixtureMessages+fixtureActivity {
		t.Fatalf("retry: %v %+v %d", err, r.Messages, len(ch))
	}
}

func TestPanicInMappingIsContained(t *testing.T) {
	tmp := isolateTmp(t)
	hookFlush(t, 10, func(kind string, _ int) error { panic("boom in " + kind) })
	db := newDB(t)
	_, _, err := Run(context.Background(), Options{Root: fixtureRoot, DBPath: db})
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.CodeInternal || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if left := snapshotDirs(t, tmp); len(left) != 0 {
		t.Fatalf("snapshot left behind: %v", left)
	}
	if st := readStatus(t, db); st.LastRun == nil || st.LastRun.Status != "failed" {
		t.Fatalf("last run: %+v", st.LastRun)
	}
	// The lock was released: a clean run works.
	batchSize, beforeFlush = 2000, func(string, int) error { return nil }
	if r, _ := run(t, Options{Root: fixtureRoot, DBPath: db}); r.Status != "ok" {
		t.Fatalf("status = %q", r.Status)
	}
}

// twoSourceRoot is a copy of the fixture with a second Teams origin (same data), plus an
// IndexedDB origin that is not Teams.
func twoSourceRoot(t *testing.T) (root, second string) {
	t.Helper()
	root = fixtureCopy(t)
	idb, _ := filepath.Glob(filepath.Join(root, "*", "IndexedDB"))
	if len(idb) != 1 {
		t.Fatalf("IndexedDB dirs: %v", idb)
	}
	for _, suffix := range []string{".indexeddb.leveldb", ".indexeddb.blob"} {
		copyTree(t, filepath.Join(idb[0], "https_teams.microsoft.com_0"+suffix), filepath.Join(idb[0], "https_teams.cloud.microsoft_0"+suffix))
	}
	if err := os.MkdirAll(filepath.Join(idb[0], "https_example.com_0.indexeddb.leveldb"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(idb[0], "https_teams.cloud.microsoft_0.indexeddb.leveldb")
}

func sourceNamed(t *testing.T, r Report, origin string) SourceReport {
	t.Helper()
	for _, s := range r.Sources {
		if strings.HasSuffix(s.Source, "|"+origin) {
			return s
		}
	}
	t.Fatalf("no source %s in %+v", origin, r.Sources)
	return SourceReport{}
}

func TestMultiSourceReport(t *testing.T) {
	root, second := twoSourceRoot(t)
	db := newDB(t)
	r, _ := run(t, Options{Root: root, DBPath: db})
	if !reflect.DeepEqual(r.OtherOrigins, []string{"https_example.com_0"}) {
		t.Fatalf("OtherOrigins = %v", r.OtherOrigins)
	}
	if len(r.Sources) != 2 || r.Status != "ok" || sourceNamed(t, r, "https_teams.microsoft.com_0").Status != "ok" || sourceNamed(t, r, "https_teams.cloud.microsoft_0").Status != "ok" {
		t.Fatalf("report: %+v", r)
	}
	// The second origin holds the same rows: seen twice, inserted once.
	if r.Messages.Seen != 2*fixtureMessages || r.Messages.Inserted != fixtureMessages || r.Messages.Unchanged != fixtureMessages {
		t.Fatalf("messages: %+v", r.Messages)
	}

	// Nothing changed: every source is skipped.
	if r, _ := run(t, Options{Root: root, DBPath: db}); r.Status != "unchanged" || len(r.Sources) != 2 {
		t.Fatalf("unchanged: %+v", r)
	}

	// Only the second source changes: the overall run decoded something, the other source skipped.
	later := time.Now().Add(time.Hour)
	logs, _ := filepath.Glob(filepath.Join(second, "*.log"))
	if len(logs) != 1 {
		t.Fatalf("logs: %v", logs)
	}
	_ = os.Chtimes(logs[0], later, later)
	r, _ = run(t, Options{Root: root, DBPath: db})
	if r.Status != "ok" || sourceNamed(t, r, "https_teams.microsoft.com_0").Status != "unchanged" || sourceNamed(t, r, "https_teams.cloud.microsoft_0").Status != "ok" {
		t.Fatalf("mixed: %+v", r)
	}
	if r.Messages.Seen != fixtureMessages || r.Messages.Unchanged != fixtureMessages {
		t.Fatalf("mixed counts: %+v", r.Messages)
	}
}

func TestMultiSourceOmissionsSum(t *testing.T) {
	rel := omittingBlob(t)
	root, _ := twoSourceRoot(t)
	blobs, _ := filepath.Glob(filepath.Join(root, "*", "IndexedDB", "*.blob"))
	if len(blobs) != 2 {
		t.Fatalf("blob dirs: %v", blobs)
	}
	for _, b := range blobs {
		if err := os.Remove(filepath.Join(b, rel)); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := run(t, Options{Root: root, DBPath: newDB(t)})
	if r.Status != "ok_with_omissions" || r.Omissions["blob_missing"] != 2 {
		t.Fatalf("status %q omissions %v", r.Status, r.Omissions)
	}
	for _, s := range r.Sources {
		if s.Status != "ok_with_omissions" || s.Omissions["blob_missing"] != 1 {
			t.Fatalf("source: %+v", s)
		}
	}
	// Only one source with omissions still makes the whole run ok_with_omissions.
	root2, _ := twoSourceRoot(t)
	b2, _ := filepath.Glob(filepath.Join(root2, "*", "IndexedDB", "*.blob"))
	_ = os.Remove(filepath.Join(b2[0], rel))
	r, _ = run(t, Options{Root: root2, DBPath: newDB(t)})
	if r.Status != "ok_with_omissions" || r.Omissions["blob_missing"] != 1 {
		t.Fatalf("one source: %q %v", r.Status, r.Omissions)
	}
}
