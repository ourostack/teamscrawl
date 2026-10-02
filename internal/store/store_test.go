package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ourostack/teamscrawl/internal/errs"
	"github.com/ourostack/teamscrawl/internal/teamsdesktop"
)

func TestSchemaModes(t *testing.T) {
	ctx := context.Background()
	t.Run("custom parent is created 0700 and file 0600", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "new", "teamscrawl.db")
		s := must(Open(ctx, p))
		defer func() { _ = s.Close() }()
		di := must(os.Stat(filepath.Dir(p)))
		fi := must(os.Stat(p))
		if runtime.GOOS == "windows" {
			assertCurrentUserAndSystemOnly(t, filepath.Dir(p))
			assertCurrentUserAndSystemOnly(t, p)
			return
		}
		if di.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
			t.Fatalf("dir %v file %v", di.Mode().Perm(), fi.Mode().Perm())
		}
	})
	t.Run("existing custom parent is left alone", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows now rejects unsafe existing custom parents before SQLite open")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: test needs a loose dir
			t.Fatal(err)
		}
		s := must(Open(ctx, filepath.Join(dir, "x.db")))
		defer func() { _ = s.Close() }()
		if m := must(os.Stat(dir)).Mode().Perm(); m != 0o755 {
			t.Fatalf("custom parent changed to %v", m)
		}
	})
	t.Run("existing default dir is tightened", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		def := filepath.Join(home, ".teamscrawl")
		if err := os.Mkdir(def, 0o755); err != nil { //nolint:gosec // G301: test needs a loose dir
			t.Fatal(err)
		}
		s := must(Open(ctx, filepath.Join(def, "teamscrawl.db")))
		defer func() { _ = s.Close() }()
		if runtime.GOOS == "windows" {
			assertCurrentUserAndSystemOnly(t, def)
			assertCurrentUserAndSystemOnly(t, filepath.Join(def, "teamscrawl.db"))
			return
		}
		if m := must(os.Stat(def)).Mode().Perm(); m != 0o700 {
			t.Fatalf("default dir is %v", m)
		}
	})
}

func TestIdempotentApply(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	must0(s.ApplyAccount(ctx, acctA))
	convs := []teamsdesktop.Conversation{conv(acctA, "c1", "Chat", "One"), conv(acctA, "c2", "Chat", "Two")}
	msgs := []teamsdesktop.Message{msg(acctA, "c1", "m1", "hello", base), msg(acctA, "c1", "m2", "world", base.Add(time.Minute)), msg(acctA, "c2", "m3", "x", base)}
	people := []teamsdesktop.Person{{TenantID: acctA.TenantID, ID: "8:orgid:p1", DisplayName: "Pat", SeenAt: base}}
	acts := []teamsdesktop.Activity{{TenantID: acctA.TenantID, UserID: acctA.UserID, ID: "a1", Type: "mention", At: base, ConversationID: "c1", MessageID: "m1"}}

	check := func(name string, got Counts, want Counts) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: got %+v want %+v", name, got, want)
		}
	}
	check("conv1", must(s.ApplyConversations(ctx, convs)), Counts{Seen: 2, Inserted: 2})
	check("msg1", must(s.ApplyMessages(ctx, msgs)), Counts{Seen: 3, Inserted: 3})
	check("ppl1", must(s.ApplyPeople(ctx, people)), Counts{Seen: 1, Inserted: 1})
	check("act1", must(s.ApplyActivity(ctx, acts)), Counts{Seen: 1, Inserted: 1})
	check("conv2", must(s.ApplyConversations(ctx, convs)), Counts{Seen: 2, Unchanged: 2})
	check("msg2", must(s.ApplyMessages(ctx, msgs)), Counts{Seen: 3, Unchanged: 3})
	check("ppl2", must(s.ApplyPeople(ctx, people)), Counts{Seen: 1, Unchanged: 1})
	check("act2", must(s.ApplyActivity(ctx, acts)), Counts{Seen: 1, Unchanged: 1})
	// Same version but changed content (a reaction arrived) updates.
	msgs[0].Reactions = []teamsdesktop.Reaction{{Key: "like", Count: 1, UserIDs: []string{"8:orgid:p1"}}}
	check("msg3", must(s.ApplyMessages(ctx, msgs)), Counts{Seen: 3, Updated: 1, Unchanged: 2})
}

func TestNewerVersionUpdates(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	m := msg(acctA, "c1", "m1", "first draft", base)
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{m}))
	m.Version = 2
	m.ContentText = "second draft"
	m.EditedAt = base.Add(time.Hour)
	c := must(s.ApplyMessages(ctx, []teamsdesktop.Message{m}))
	if c != (Counts{Seen: 1, Updated: 1}) {
		t.Fatalf("counts %+v", c)
	}
	rows, _ := must2(s.Messages(ctx, Filter{}))
	if len(rows) != 1 || rows[0].ContentText != "second draft" || rows[0].EditedAt.IsZero() || rows[0].Version != 2 {
		t.Fatalf("rows %+v", rows)
	}
	// The old text must be gone from the index, the new text findable.
	if hits, _ := must2(s.Search(ctx, "first", Filter{})); len(hits) != 0 {
		t.Fatalf("stale FTS hit: %+v", hits)
	}
	if hits, _ := must2(s.Search(ctx, "second", Filter{})); len(hits) != 1 {
		t.Fatalf("missing FTS hit")
	}
}

func TestOlderVersionIgnored(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	m := msg(acctA, "c1", "m1", "new text", base)
	m.Version = 5
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{m}))
	m.Version = 3
	m.ContentText = "stale text"
	c := must(s.ApplyMessages(ctx, []teamsdesktop.Message{m}))
	if c != (Counts{Seen: 1, Unchanged: 1}) {
		t.Fatalf("counts %+v", c)
	}
	rows, _ := must2(s.Messages(ctx, Filter{}))
	if rows[0].ContentText != "new text" || rows[0].Version != 5 {
		t.Fatalf("row %+v", rows[0])
	}
}

func TestDeletedExcludedByDefault(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	live := msg(acctA, "c1", "m1", "deploy live", base)
	dead := msg(acctA, "c1", "m2", "deploy dead", base.Add(time.Minute))
	dead.DeletedAt = base.Add(time.Hour)
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{live, dead}))

	rows, _ := must2(s.Messages(ctx, Filter{}))
	if !eqStrings(ids(rows), []string{"m1"}) {
		t.Fatalf("default messages %v", ids(rows))
	}
	rows, _ = must2(s.Messages(ctx, Filter{IncludeDeleted: true}))
	if !eqStrings(ids(rows), []string{"m1", "m2"}) || rows[1].DeletedAt.IsZero() {
		t.Fatalf("include-deleted messages %v", ids(rows))
	}
	hits, _ := must2(s.Search(ctx, "deploy", Filter{}))
	if len(hits) != 1 {
		t.Fatalf("search default %v", ids(hits))
	}
	hits, _ = must2(s.Search(ctx, "deploy", Filter{IncludeDeleted: true}))
	if len(hits) != 2 {
		t.Fatalf("search include deleted %v", ids(hits))
	}
	// A later deletion of an already stored message updates it.
	live.Version = 2
	live.DeletedAt = base.Add(2 * time.Hour)
	c := must(s.ApplyMessages(ctx, []teamsdesktop.Message{live}))
	if c.Updated != 1 {
		t.Fatalf("counts %+v", c)
	}
	rows, _ = must2(s.Messages(ctx, Filter{}))
	if len(rows) != 0 {
		t.Fatalf("deleted still listed %v", ids(rows))
	}
}

func TestSameConversationTwoAccounts(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	must(s.ApplyConversations(ctx, []teamsdesktop.Conversation{conv(acctA, "shared", "Chat", "Shared A"), conv(acctB, "shared", "Chat", "Shared B")}))
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{msg(acctA, "shared", "m1", "from a", base), msg(acctB, "shared", "m1", "from b", base)}))

	all, _ := must2(s.Messages(ctx, Filter{}))
	if len(all) != 2 {
		t.Fatalf("want 2 rows got %d", len(all))
	}
	onlyA, _ := must2(s.Messages(ctx, Filter{Account: &acctA}))
	if len(onlyA) != 1 || onlyA[0].ContentText != "from a" || onlyA[0].ConversationDisplayName != "Shared A" {
		t.Fatalf("A: %+v", onlyA)
	}
	onlyB, _ := must2(s.Search(ctx, "from", Filter{Account: &acctB}))
	if len(onlyB) != 1 || onlyB[0].ContentText != "from b" {
		t.Fatalf("B: %+v", onlyB)
	}
	convs, _ := must2(s.Conversations(ctx, "", "", Filter{}))
	if len(convs) != 2 {
		t.Fatalf("convs %d", len(convs))
	}
	convs, _ = must2(s.Conversations(ctx, "", "", Filter{Account: &acctB}))
	if len(convs) != 1 || convs[0].DisplayName != "Shared B" {
		t.Fatalf("convs B %+v", convs)
	}
}

func TestOpenReadOnlyMissing(t *testing.T) {
	_, err := OpenReadOnly(context.Background(), filepath.Join(t.TempDir(), "nope.db"))
	if !errors.Is(err, ErrNoArchive) {
		t.Fatalf("got %v", err)
	}
}

func TestReadOnlySQLRejectsWrites(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "data", "a.db")
	w := must(Open(ctx, p))
	must(w.ApplyMessages(ctx, []teamsdesktop.Message{msg(acctA, "c", "m1", "hi", base)}))
	must0(w.Close())

	ro := must(OpenReadOnly(ctx, p))
	defer func() { _ = ro.Close() }()
	cols, rows, _, err := ro.SQL(ctx, "select count(*) as n from messages", 10)
	if err != nil || len(cols) != 1 || cols[0] != "n" || len(rows) != 1 || rows[0][0] != int64(1) {
		t.Fatalf("select: %v %v %v", cols, rows, err)
	}
	for _, q := range []string{"delete from messages", "insert into people(tenant_id,id) values('a','b')", "drop table messages", "pragma query_only=0; delete from messages", "attach database '/etc/hosts' as x"} {
		if _, _, _, err := ro.SQL(ctx, q, 10); err == nil {
			t.Fatalf("write accepted: %s", q)
		}
	}
	if _, rows, _, _ := ro.SQL(ctx, "select count(*) from messages", 10); rows[0][0] != int64(1) {
		t.Fatalf("data changed")
	}
	// A writable store refuses SQL too: the escape hatch is read-only by construction.
	rw := must(Open(ctx, filepath.Join(t.TempDir(), "data", "b.db")))
	defer func() { _ = rw.Close() }()
	if _, _, _, err := rw.SQL(ctx, "select 1", 10); err == nil {
		t.Fatal("SQL allowed on writable store")
	}
}

func TestReadOnlyWhileWriterActive(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "data", "a.db")
	w := must(Open(ctx, p))
	defer func() { _ = w.Close() }()
	must(w.ApplyMessages(ctx, []teamsdesktop.Message{msg(acctA, "c", "m1", "hi", base)}))
	ro := must(OpenReadOnly(ctx, p))
	defer func() { _ = ro.Close() }()
	// Write while the reader is open: the reader must see it.
	must(w.ApplyMessages(ctx, []teamsdesktop.Message{msg(acctA, "c", "m2", "there", base.Add(time.Minute))}))
	_, rows, _, err := ro.SQL(ctx, "select count(*) from messages", 10)
	if err != nil || rows[0][0] != int64(2) {
		t.Fatalf("%v %v", rows, err)
	}
	if hits, _ := must2(ro.Search(ctx, "there", Filter{})); len(hits) != 1 {
		t.Fatal("search on read-only store failed")
	}
}

func TestLockHeld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "a.db")
	rel, err := AcquireLock(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err2 := AcquireLock(p)
	var c *errs.Coded
	if !errors.As(err2, &c) || c.Code != errs.CodeLocked || c.Exit != errs.ExitLocked {
		t.Fatalf("second lock: %v", err2)
	}
	rel()
	rel() // idempotent
	rel2, err := AcquireLock(p)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel2()
}

func TestRunsAndFingerprint(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if fp := must(s.LastFingerprint(ctx, "src")); fp != "" {
		t.Fatalf("fp %q", fp)
	}
	t0 := base
	must0(s.RecordRun(ctx, Run{StartedAt: t0, FinishedAt: t0.Add(time.Second), Source: "src", Fingerprint: "fp1", Status: "ok", Counts: map[string]int{"messages": 1}}))
	must0(s.RecordRun(ctx, Run{StartedAt: t0.Add(time.Minute), FinishedAt: t0.Add(time.Minute), Source: "src", Fingerprint: "fp2", Status: "failed"}))
	must0(s.RecordRun(ctx, Run{StartedAt: t0.Add(2 * time.Minute), FinishedAt: t0.Add(2 * time.Minute), Source: "other", Fingerprint: "zz", Status: "ok", Accounts: []string{"*"}}))
	if fp := must(s.LastFingerprint(ctx, "src")); fp != "fp1" {
		t.Fatalf("failed runs must not count, got %q", fp)
	}
	st := must(s.Status(ctx))
	if st.LastRun == nil || st.LastRun.Source != "other" || st.LastRun.Status != "ok" {
		t.Fatalf("last run %+v", st.LastRun)
	}
	if !st.LastSuccessAt.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("last success %v", st.LastSuccessAt)
	}
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	empty := must(s.Status(ctx))
	if empty.SchemaVersion != SchemaVersion || !empty.FTSPresent || len(empty.Accounts) != 0 || empty.LastRun != nil || !empty.LastSuccessAt.IsZero() {
		t.Fatalf("empty %+v", empty)
	}
	must0(s.ApplyAccount(ctx, acctA))
	must(s.ApplyConversations(ctx, []teamsdesktop.Conversation{conv(acctA, "c1", "Chat", "x")}))
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{msg(acctA, "c1", "m1", "a", base), msg(acctA, "c1", "m2", "b", base.Add(time.Hour))}))
	must(s.ApplyPeople(ctx, []teamsdesktop.Person{{TenantID: acctA.TenantID, ID: "8:orgid:p", DisplayName: "P"}}))
	must(s.ApplyActivity(ctx, []teamsdesktop.Activity{{TenantID: acctA.TenantID, UserID: acctA.UserID, ID: "a", At: base}}))
	st := must(s.Status(ctx))
	if len(st.Accounts) != 1 {
		t.Fatalf("%+v", st)
	}
	a := st.Accounts[0]
	if a.Conversations != 1 || a.Messages != 2 || a.People != 1 || a.Activity != 1 || !a.NewestSentAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("account %+v", a)
	}
	if !st.NewestSentAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("newest %v", st.NewestSentAt)
	}
}
