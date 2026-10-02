package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ourostack/teamscrawl/internal/teamsdesktop"
)

func TestCheckSQL(t *testing.T) {
	good := []string{
		"select 1",
		"  SELECT 1;  ",
		"with x as (select 1) select * from x",
		"explain select 1",
		"values (1),(2)",
		"-- a comment; with a semicolon\nselect 1",
		"/* a ; b */ select 1 /* trailing */",
		"select 'a;b' as x, \"c;d\" as y, `e;f` as z, [g;h] as w",
		"select 'it''s; fine'",
		"select 'attach' as detach",
		"select 1 -- ; attach database 'x' as y",
		"select 1;  -- nothing follows\n",
		"select 1; /* nothing */",
	}
	for _, q := range good {
		if err := CheckSQL(q); err != nil {
			t.Errorf("CheckSQL(%q) = %v", q, err)
		}
	}
	bad := map[string]string{
		"":                                       "only read statements",
		"   ":                                    "only read statements",
		"-- only a comment":                      "only read statements",
		"/* unterminated select 1":               "only read statements",
		"delete from messages":                   "only read statements",
		"attach database 'x' as y":               "only read statements",
		"detach database y":                      "only read statements",
		"pragma query_only=0":                    "only read statements",
		"select 1; select 2":                     "single statement",
		"select 1; /* c */ delete from messages": "single statement",
		"select 1;\n-- c\nselect 2":              "single statement",
		"1select":                                "only read statements",
	}
	for q, want := range bad {
		err := CheckSQL(q)
		if err == nil || !errors.Is(err, ErrQueryRefused) || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckSQL(%q) = %v, want refusal containing %q", q, err, want)
		}
	}
}

func TestSQLStreamsAndStopsAtTheLimit(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "data", "a.db")
	w := must(Open(ctx, p))
	must(w.ApplyMessages(ctx, []teamsdesktop.Message{
		msg(acctA, "c", "m1", "has an attach word", base),
		msg(acctA, "c", "m2", "plain", base.Add(1)),
		msg(acctA, "c", "m3", "plain", base.Add(2)),
	}))
	must0(w.Close())
	ro := must(OpenReadOnly(ctx, p))
	defer func() { _ = ro.Close() }()

	cols, rows, truncated, err := ro.SQL(ctx, "select id from messages order by id", 2)
	if err != nil || !truncated || len(rows) != 2 || len(cols) != 1 || rows[1][0] != "m2" {
		t.Fatalf("limit 2: %v %v %v %v", cols, rows, truncated, err)
	}
	_, rows, truncated, err = ro.SQL(ctx, "select id from messages", 3)
	if err != nil || truncated || len(rows) != 3 {
		t.Fatalf("limit 3: %v %v %v", rows, truncated, err)
	}
	// The word attach inside a string literal is data, not a statement.
	_, rows, _, err = ro.SQL(ctx, "select id from messages where content_text like '%attach%'", 10)
	if err != nil || len(rows) != 1 || rows[0][0] != "m1" {
		t.Fatalf("literal attach: %v %v", rows, err)
	}
	// Blobs come back as strings.
	_, rows, _, err = ro.SQL(ctx, "select cast('x' as blob)", 1)
	if err != nil || rows[0][0] != "x" {
		t.Fatalf("blob: %v %v", rows, err)
	}
	// An engine error is not a refusal.
	_, _, _, err = ro.SQL(ctx, "select * from no_such_table", 1)
	if err == nil || errors.Is(err, ErrQueryRefused) || !strings.Contains(err.Error(), "no_such_table") {
		t.Fatalf("engine error: %v", err)
	}
	// A statement SQLite refuses on a read-only file stays an engine error: the connection is the guard.
	_, _, _, err = ro.SQL(ctx, "with x as (select 1) insert into people(tenant_id,id) values('a','b')", 1)
	if err == nil {
		t.Fatal("write accepted")
	}
	// Refusals come first.
	if _, _, _, err := ro.SQL(ctx, "attach database ':memory:' as x", 1); !errors.Is(err, ErrQueryRefused) {
		t.Fatalf("attach: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, err := ro.SQL(cctx, "select 1", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	rw := must(Open(ctx, filepath.Join(t.TempDir(), "data", "b.db")))
	defer func() { _ = rw.Close() }()
	if _, _, _, err := rw.SQL(ctx, "select 1", 1); err == nil {
		t.Fatal("SQL allowed on writable store")
	}
}
