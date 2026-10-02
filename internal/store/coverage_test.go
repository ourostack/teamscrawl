package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ourostack/teamscrawl/internal/errs"
	"github.com/ourostack/teamscrawl/internal/teamsdesktop"
)

func codedAs(t *testing.T, err error, code string) {
	t.Helper()
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != code {
		t.Fatalf("want coded %s, got %v", code, err)
	}
}

func TestAcquireLockFailures(t *testing.T) {
	dir := t.TempDir()
	t.Run("parent that is a file", func(t *testing.T) {
		file := filepath.Join(dir, "afile")
		must0(os.WriteFile(file, nil, 0o600))
		_, err := AcquireLock(filepath.Join(file, "x.db"))
		codedAs(t, err, errs.CodeDBError)
	})
	t.Run("lock path that is a directory", func(t *testing.T) {
		p := filepath.Join(dir, "d.db")
		must0(os.Mkdir(p+".lock", 0o700))
		_, err := AcquireLock(p)
		codedAs(t, err, errs.CodeDBError)
	})
}

func TestOpenFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("parent that is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "afile")
		must0(os.WriteFile(file, nil, 0o600))
		if _, err := Open(ctx, filepath.Join(file, "x.db")); err == nil || !strings.Contains(err.Error(), "create archive dir") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("path that is a directory", func(t *testing.T) {
		if _, err := Open(ctx, t.TempDir()); err == nil {
			t.Fatal("opening a directory as the archive succeeded")
		}
	})
	t.Run("chmod of the file fails", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows uses ACL finalization instead of chmod")
		}
		boom := errors.New("boom")
		old := chmodFile
		chmodFile = func(string, os.FileMode) error { return boom }
		t.Cleanup(func() { chmodFile = old })
		_, err := Open(ctx, filepath.Join(t.TempDir(), "x.db"))
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "chmod archive") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("chmod of the default dir fails", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows uses ACL finalization instead of chmod")
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		boom := errors.New("boom")
		old := chmodFile
		chmodFile = func(string, os.FileMode) error { return boom }
		t.Cleanup(func() { chmodFile = old })
		err := ensureParent(filepath.Join(home, ".teamscrawl", "x.db"))
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "chmod archive dir") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a failed migration closes the archive and reports", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "data", "old.db")
		must0(os.MkdirAll(filepath.Dir(p), 0o700))
		if runtime.GOOS == "windows" {
			setCurrentUserAndSystemOnly(t, filepath.Dir(p))
		}
		raw := must(sql.Open("sqlite", p))
		// An archive that has both the old and the new column name: the rename must fail.
		_, err := raw.Exec(`create table conversations(tenant_id text, user_id text, id text, team_id text, last_message_at text, read_horizon_message_id text, read_horizon_client_message_id text)`)
		must0(err)
		must0(raw.Close())
		if runtime.GOOS == "windows" {
			setCurrentUserAndSystemOnly(t, p)
		}
		if _, err := Open(ctx, p); err == nil {
			t.Fatal("Open succeeded over an archive whose migration cannot run")
		}
	})
	t.Run("migrate on an unusable connection", func(t *testing.T) {
		s := newStore(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.migrate(cctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMigrateRenamesOldColumn(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "data", "old.db")
	must0(os.MkdirAll(filepath.Dir(p), 0o700))
	if runtime.GOOS == "windows" {
		setCurrentUserAndSystemOnly(t, filepath.Dir(p))
	}
	raw := must(sql.Open("sqlite", p))
	_, err := raw.Exec(`create table conversations(tenant_id text not null, user_id text not null, id text not null, team_id text not null default '', last_message_at text, read_horizon_message_id text not null default '')`)
	must0(err)
	must0(raw.Close())
	if runtime.GOOS == "windows" {
		setCurrentUserAndSystemOnly(t, p)
	}
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var n int
	must0(s.db.QueryRow(`select count(*) from pragma_table_info('conversations') where name='read_horizon_client_message_id'`).Scan(&n))
	if n != 1 {
		t.Fatal("old column was not renamed")
	}
}

func TestOpenReadOnlyFailures(t *testing.T) {
	ctx := context.Background()
	if _, err := OpenReadOnly(ctx, filepath.Join(t.TempDir(), "none.db")); !errors.Is(err, ErrNoArchive) {
		t.Fatalf("missing file: %v", err)
	}
	// A directory exists but cannot be opened as a database.
	if _, err := OpenReadOnly(ctx, t.TempDir()); err == nil || errors.Is(err, ErrNoArchive) {
		t.Fatalf("directory: %v", err)
	}
}

func TestParseTime(t *testing.T) {
	if got := parseTime(sql.NullString{String: "not a time", Valid: true}); !got.IsZero() {
		t.Fatalf("garbage parsed to %v", got)
	}
	want := base
	if got := parseTime(sql.NullString{String: want.Format(timeLayout), Valid: true}); !got.Equal(want) {
		t.Fatalf("got %v", got)
	}
}

func TestRecordRunRejectsUnmarshalableCounts(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.RecordRun(ctx, Run{StartedAt: base, Status: "ok", Counts: make(chan int)}); err == nil {
		t.Fatal("unmarshalable counts were recorded")
	}
	var n int
	must0(s.db.QueryRow(`select count(*) from sync_runs`).Scan(&n))
	if n != 0 {
		t.Fatalf("a rejected run left %d rows", n)
	}
}

func TestStatusSchemaVersionFailure(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	_, err := s.db.Exec(`drop table schema_migrations; create table schema_migrations(other integer)`)
	must0(err)
	if _, err := s.Status(ctx); err == nil {
		t.Fatal("Status hid an unreadable schema version")
	}
}

func TestApplyRejectsRowsWithoutKeys(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	snapshot := dump(t, s)
	if err := s.ApplyAccount(ctx, teamsdesktop.Account{TenantID: "t"}); err == nil {
		t.Fatal("account without user id accepted")
	}
	if _, err := s.ApplyConversations(ctx, []teamsdesktop.Conversation{conv(acctA, "ok", "Chat", "x"), {TenantID: "t", UserID: "u"}}); err == nil {
		t.Fatal("conversation without id accepted")
	}
	if _, err := s.ApplyPeople(ctx, []teamsdesktop.Person{{TenantID: "t"}}); err == nil {
		t.Fatal("person without id accepted")
	}
	if _, err := s.ApplyMessages(ctx, []teamsdesktop.Message{{TenantID: "t", UserID: "u", ID: "m"}}); err == nil {
		t.Fatal("message without conversation accepted")
	}
	if _, err := s.ApplyActivity(ctx, []teamsdesktop.Activity{{TenantID: "t", UserID: "u"}}); err == nil {
		t.Fatal("activity without id accepted")
	}
	if after := dump(t, s); after != snapshot {
		t.Fatal("a rejected batch left rows behind (the valid conversation before the bad one must roll back)")
	}
}

func TestZeroTimesStoreAsEmpty(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	m := msg(acctA, "c", "m", "no time", time.Time{})
	a := teamsdesktop.Activity{TenantID: acctA.TenantID, UserID: acctA.UserID, ID: "a"}
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{m}))
	must(s.ApplyActivity(ctx, []teamsdesktop.Activity{a}))
	var sent, at string
	must0(s.db.QueryRow(`select sent_at from messages`).Scan(&sent))
	must0(s.db.QueryRow(`select at from activity`).Scan(&at))
	if sent != "" || at != "" {
		t.Fatalf("sent %q at %q", sent, at)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmpty("", "b", "c"); got != "b" {
		t.Fatalf("got %q", got)
	}
}

func TestByKeySkipsMalformedKeys(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	rows, err := s.MessagesByKey(ctx, []string{"malformed", "a|b"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("messages: %v %v", rows, err)
	}
	acts, err := s.ActivityByKey(ctx, []string{"malformed", "a|b"})
	if err != nil || len(acts) != 0 {
		t.Fatalf("activity: %v %v", acts, err)
	}
}

func TestMessageHTMLNoKeys(t *testing.T) {
	got, err := newStore(t).MessageHTML(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestReadFilterFailuresSurface(t *testing.T) {
	// A From filter probes the archive first; that probe failing must fail the read.
	f := Filter{From: "Pat"}
	sweepReadFaults(t, seedReadable, func(ctx context.Context, s *Store) error { _, _, err := s.Unread(ctx, f); return err })
	sweepReadFaults(t, seedReadable, func(ctx context.Context, s *Store) error { _, _, err := s.UnreadByConversation(ctx, f); return err })
}

func TestQueryTruncationAndUsage(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedReadable(t, s)
	_, trunc, err := s.Thread(ctx, "c1", "m1", Filter{Limit: 1})
	if err != nil || !trunc {
		t.Fatalf("thread truncation: %v %v", trunc, err)
	}
	cs, trunc, err := s.Conversations(ctx, "", "", Filter{Limit: 1})
	if err != nil || !trunc || len(cs) != 1 {
		t.Fatalf("conversations truncation: %v %v %v", cs, trunc, err)
	}
	ps, trunc, err := s.People(ctx, "", Filter{Limit: 1})
	if err != nil || !trunc || len(ps) != 1 {
		t.Fatalf("people truncation: %v %v %v", ps, trunc, err)
	}
	_, _, err = s.Conversations(ctx, "", "***", Filter{})
	codedAs(t, err, errs.CodeUsage)
	_, _, err = s.Search(ctx, "**", Filter{})
	codedAs(t, err, errs.CodeUsage)
}
