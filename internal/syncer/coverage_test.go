package syncer

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ourostack/teamscrawl/internal/errs"
	"github.com/ourostack/teamscrawl/internal/store"
	"github.com/ourostack/teamscrawl/internal/teamsdesktop"
)

// archiveWith creates an archive and then runs ddl against it, so a sync over it meets the schema
// change (a trigger that fails a write, a renamed column, ...).
func archiveWith(t *testing.T, ddl string) string {
	t.Helper()
	db := newDB(t)
	s, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if ddl != "" {
		raw, err := sql.Open("sqlite", db)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		if _, err := raw.Exec(ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	return db
}

func codedErr(t *testing.T, err error, code string) {
	t.Helper()
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != code {
		t.Fatalf("want coded %s, got %v", code, err)
	}
}

// requireNothingKept checks the archive holds no rows from a failed sync and that the attempt is
// recorded as failed.
func requireNothingKept(t *testing.T, db string) {
	t.Helper()
	st := readStatus(t, db)
	if c, m, a := totals(st); c+m+a != 0 || len(st.Accounts) != 0 {
		t.Fatalf("a failed sync kept rows: %d %d %d %+v", c, m, a, st.Accounts)
	}
	if st.LastRun == nil || st.LastRun.Status != "failed" || !st.LastSuccessAt.IsZero() {
		t.Fatalf("last run: %+v", st.LastRun)
	}
}

func TestStoreFailuresAreCodedAndRollBack(t *testing.T) {
	abort := func(table string) string {
		return `create trigger boom before insert on ` + table + ` begin select raise(abort, 'injected'); end;`
	}
	cases := map[string]string{
		"account insert":      abort("accounts"),
		"conversation insert": abort("conversations"),
		"message insert":      abort("messages"),
		"activity insert":     abort("activity"),
		"people insert":       abort("people"),
		"run record":          `create trigger boom before insert on sync_runs when new.status in ('ok','ok_with_omissions') begin select raise(abort, 'injected'); end;`,
		// A deferred foreign key is only checked at COMMIT, so this fails the commit itself.
		"commit": `create table fkp(id integer primary key);
create table fkc(pid integer references fkp(id) deferrable initially deferred);
create trigger boom after insert on sync_runs when new.status in ('ok','ok_with_omissions') begin insert into fkc(pid) values(1); end;`,
	}
	for name, ddl := range cases {
		t.Run(name, func(t *testing.T) {
			isolateTmp(t)
			db := archiveWith(t, ddl)
			r, ch, err := Run(context.Background(), Options{Root: fixtureRoot, DBPath: db})
			codedErr(t, err, errs.CodeDBError)
			if !strings.Contains(err.Error(), "injected") && name != "commit" {
				t.Fatalf("cause lost: %v", err)
			}
			if r.Status != StatusFailed || ch != nil {
				t.Fatalf("a failed run reports status failed and no changes: %+v", r)
			}
			requireNothingKept(t, db)
		})
	}
}

func TestFingerprintLookupFailure(t *testing.T) {
	isolateTmp(t)
	db := archiveWith(t, `alter table sync_runs rename column fingerprint to fp`)
	_, _, err := Run(context.Background(), Options{Root: fixtureRoot, DBPath: db})
	codedErr(t, err, errs.CodeDBError)
}

func TestUnchangedRunRecordFailure(t *testing.T) {
	isolateTmp(t)
	db := newDB(t)
	run(t, Options{Root: fixtureRoot, DBPath: db})
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`create trigger boom before insert on sync_runs when new.status = 'unchanged' begin select raise(abort, 'injected'); end;`)
	_ = raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Run(context.Background(), Options{Root: fixtureRoot, DBPath: db})
	codedErr(t, err, errs.CodeDBError)
	if st := readStatus(t, db); st.LastRun.Status != "failed" {
		t.Fatalf("last run: %+v", st.LastRun)
	}
}

func TestOpenFailureIsCoded(t *testing.T) {
	isolateTmp(t)
	// The archive path is a directory: the lock beside it is fine, the database cannot open.
	_, _, err := Run(context.Background(), Options{Root: fixtureRoot, DBPath: t.TempDir()})
	codedErr(t, err, errs.CodeDBError)
}

func TestCancelledBeforeTransactionBegins(t *testing.T) {
	tmp := isolateTmp(t)
	db := newDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	afterSnapshot = cancel
	t.Cleanup(func() { afterSnapshot = func() {} })
	_, _, err := Run(ctx, Options{Root: fixtureRoot, DBPath: db})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	var coded *errs.Coded
	if errors.As(err, &coded) {
		t.Fatalf("a cancellation is reported as such, not under the code %s", coded.Code)
	}
	if left := snapshotDirs(t, tmp); len(left) != 0 {
		t.Fatalf("snapshot left behind: %v", left)
	}
	requireNothingKept(t, db)
}

func TestCancelledBetweenSources(t *testing.T) {
	isolateTmp(t)
	root, _ := twoSourceRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := newDB(t)
	// The first source reports progress after it has committed; cancel then.
	_, _, err := Run(ctx, Options{Root: root, DBPath: db, Progress: cancelWriter(cancel)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	st := readStatus(t, db)
	if st.LastRun == nil || st.LastRun.Status != StatusPartial {
		t.Fatalf("one source committed before the cancel, so the run is partial: %+v", st.LastRun)
	}
}

type cancelWriter context.CancelFunc

func (c cancelWriter) Write(p []byte) (int, error) { c(); return len(p), nil }

func TestDefaultRootWhenNoneGiven(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	_, _, err := Run(context.Background(), Options{DBPath: newDB(t)})
	codedErr(t, err, errs.CodeTeamsNotInstalled)
	if !strings.Contains(err.Error(), home) {
		t.Fatalf("did not look under the default root: %v", err)
	}
}

func TestUnreadableSourceFailsFingerprint(t *testing.T) {
	isolateTmp(t)
	root := fixtureCopy(t)
	ldb, _ := filepath.Glob(filepath.Join(root, "*", "IndexedDB", "*.leveldb"))
	if len(ldb) != 1 {
		t.Fatalf("leveldb dirs: %v", ldb)
	}
	locked := filepath.Join(ldb[0], "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil { //nolint:gosec // G302: the test needs an unreadable directory
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // G302: restore so TempDir cleanup can remove it
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("permissions are not enforced here (running as root?)")
	}
	db := newDB(t)
	_, _, err := Run(context.Background(), Options{Root: root, DBPath: db})
	if err == nil {
		t.Fatal("an unreadable cache directory was fingerprinted")
	}
	if st := readStatus(t, db); st.LastRun == nil || st.LastRun.Status != "failed" {
		t.Fatalf("last run: %+v", st.LastRun)
	}
}

func TestSnapshotFailure(t *testing.T) {
	setTempDirEnv(t, filepath.Join(t.TempDir(), "missing"))
	db := newDB(t)
	_, _, err := Run(context.Background(), Options{Root: fixtureRoot, DBPath: db})
	codedErr(t, err, errs.CodeInternal)
	requireNothingKept(t, db)
}

// TestFlushFailuresAbortTheSource fails each kind of batch, once while records are still being
// read (batch size 1) and once at the final flush (batch size above the fixture), and requires the
// injected error to come back with nothing kept.
func TestFlushFailuresAbortTheSource(t *testing.T) {
	for _, kind := range []string{"conversation", "message", "activity"} {
		for _, size := range []int{1, 100000} {
			t.Run(kind, func(t *testing.T) {
				isolateTmp(t)
				injected := errors.New("injected " + kind + " flush failure")
				hookFlush(t, size, func(k string, _ int) error {
					if k == kind {
						return injected
					}
					return nil
				})
				db := newDB(t)
				_, _, err := Run(context.Background(), Options{Root: fixtureRoot, DBPath: db})
				if !errors.Is(err, injected) {
					t.Fatalf("err = %v", err)
				}
				requireNothingKept(t, db)
			})
		}
	}
}

func TestWriterFlushesNothingWhenEmpty(t *testing.T) {
	hookFlush(t, 10, func(k string, _ int) error { return errors.New("flushed an empty batch: " + k) })
	w := &writer{ctx: context.Background()}
	for name, flush := range map[string]func() error{"conversations": w.flushConversations, "messages": w.flushMessages, "activity": w.flushActivity} {
		if err := flush(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWriterRejectsUnmappableRecords(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, newDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	sess, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Rollback()
	w := &writer{ctx: ctx, sess: sess, seenAcct: map[[2]string]bool{}, people: map[[2]string]teamsdesktop.Person{}}
	for _, kind := range []string{teamsdesktop.KindReplyChain, teamsdesktop.KindConversation, teamsdesktop.KindActivity} {
		err := w.add(acctA, kind, "not an object")
		var u *teamsdesktop.UnmappedError
		if !errors.As(err, &u) {
			t.Errorf("%s: err = %v", kind, err)
		}
	}
	if n := w.counts.Messages.Seen + w.counts.Conversations.Seen + w.counts.Activity.Seen; n != 0 {
		t.Fatalf("unmappable records were counted: %+v", w.counts)
	}
}

func TestWriterOnFinishedSessionReportsArchiveErrors(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, newDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	sess, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sess.Rollback() // every later write on the session fails
	newWriter := func() *writer {
		return &writer{ctx: ctx, sess: sess, seenAcct: map[[2]string]bool{}, people: map[[2]string]teamsdesktop.Person{
			{"t", "p"}: {TenantID: "t", ID: "p"},
		}}
	}
	if err := newWriter().add(acctA, "other", nil); err == nil {
		t.Error("add on a finished session succeeded")
	} else {
		codedErr(t, err, errs.CodeDBError)
	}
	for name, w := range map[string]*writer{
		"conversations": {ctx: ctx, sess: sess, convs: []teamsdesktop.Conversation{{TenantID: "t", UserID: "u", ID: "c"}}},
		"messages":      {ctx: ctx, sess: sess, msgs: []teamsdesktop.Message{{TenantID: "t", UserID: "u", ConversationID: "c", ID: "m"}}},
		"activity":      {ctx: ctx, sess: sess, acts: []teamsdesktop.Activity{{TenantID: "t", UserID: "u", ID: "a"}}},
	} {
		var err error
		switch name {
		case "conversations":
			err = w.flushConversations()
		case "messages":
			err = w.flushMessages()
		default:
			err = w.flushActivity()
		}
		if err == nil {
			t.Errorf("%s: flush on a finished session succeeded", name)
			continue
		}
		codedErr(t, err, errs.CodeDBError)
		if w.counts != (runCounts{}) {
			t.Errorf("%s: counted a failed batch: %+v", name, w.counts)
		}
	}
	err = newWriter().finish()
	codedErr(t, err, errs.CodeDBError)
}

func TestAsCoded(t *testing.T) {
	coded := errs.Usage("x")
	if got := asCoded(coded); got != error(coded) { //nolint:errorlint // identity: the error must come back unwrapped
		t.Fatalf("coded error was rewrapped: %v", got)
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := asCoded(err); got != err { //nolint:errorlint // identity: the error must come back unwrapped
			t.Fatalf("%v was rewrapped as %v", err, got)
		}
	}
	plain := errors.New("disk on fire")
	got := asCoded(plain)
	codedErr(t, got, errs.CodeDBError)
	if !errors.Is(got, plain) {
		t.Fatalf("cause lost: %v", got)
	}
}
