package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ourostack/teamscrawl/internal/teamsdesktop"
)

func TestSessionCommit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	x, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Rollback()
	// The writer has a single connection, so other connections see the archive through a read-only open.
	ro, err := OpenReadOnly(ctx, s.cs.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	must0(x.ApplyAccount(ctx, acctA))
	cc := must(x.ApplyConversations(ctx, []teamsdesktop.Conversation{conv(acctA, "c1", "Chat", "Pat")}))
	mc, changes, err := x.ApplyMessagesChanges(ctx, []teamsdesktop.Message{msg(acctA, "c1", "m1", "hello", base)})
	if err != nil || mc.Inserted != 1 || len(changes) != 1 || cc.Inserted != 1 {
		t.Fatalf("%+v %v %v", mc, changes, err)
	}
	must(x.ApplyPeople(ctx, []teamsdesktop.Person{{TenantID: acctA.TenantID, ID: "8:orgid:p", DisplayName: "P", SeenAt: base}}))
	_, _, err = x.ApplyActivityChanges(ctx, []teamsdesktop.Activity{{TenantID: acctA.TenantID, UserID: acctA.UserID, ID: "a1", At: base}})
	if err != nil {
		t.Fatal(err)
	}
	must0(x.RecordRun(ctx, Run{StartedAt: base, FinishedAt: base, Source: "s", Fingerprint: "fp", Status: "ok"}))
	if rows, _ := must2(ro.Messages(ctx, Filter{})); len(rows) != 0 {
		t.Fatalf("uncommitted rows visible to another connection: %v", ids(rows))
	}
	if fp, _ := ro.LastFingerprint(ctx, "s"); fp != "" {
		t.Fatalf("uncommitted run visible: %q", fp)
	}
	if err := x.Commit(); err != nil {
		t.Fatal(err)
	}
	x.Rollback() // no-op after commit
	if rows, _ := must2(s.Messages(ctx, Filter{})); len(rows) != 1 {
		t.Fatalf("committed rows: %v", ids(rows))
	}
	if fp, _ := s.LastFingerprint(ctx, "s"); fp != "fp" {
		t.Fatalf("run not committed: %q", fp)
	}
}

func TestSessionRollback(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	x, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	must0(x.ApplyAccount(ctx, acctA))
	must(x.ApplyConversations(ctx, []teamsdesktop.Conversation{conv(acctA, "c1", "Chat", "Pat")}))
	must(x.ApplyMessages(ctx, []teamsdesktop.Message{msg(acctA, "c1", "m1", "hello", base)}))
	must0(x.RecordRun(ctx, Run{StartedAt: base, Source: "s", Fingerprint: "fp", Status: "ok"}))
	x.Rollback()
	st := must(s.Status(ctx))
	if len(st.Accounts) != 0 || st.LastRun != nil {
		t.Fatalf("rollback left %+v", st)
	}
	if rows, _ := must2(s.Messages(ctx, Filter{})); len(rows) != 0 {
		t.Fatalf("rows survived: %v", ids(rows))
	}
	if rows, _ := must2(s.Search(ctx, "hello", Filter{})); len(rows) != 0 {
		t.Fatalf("index survived: %v", ids(rows))
	}
}

func TestChangesNilOnError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	bad := msg(acctA, "c1", "m2", "x", base)
	bad.ID = ""
	c, changes, err := s.ApplyMessagesChanges(ctx, []teamsdesktop.Message{msg(acctA, "c1", "m1", "ok", base), bad})
	if err == nil || changes != nil || c != (Counts{}) {
		t.Fatalf("%+v %v %v", c, changes, err)
	}
	a := teamsdesktop.Activity{TenantID: acctA.TenantID, UserID: acctA.UserID, ID: "a", At: base}
	_, changes, err = s.ApplyActivityChanges(ctx, []teamsdesktop.Activity{a, {}})
	if err == nil || changes != nil {
		t.Fatalf("%v %v", changes, err)
	}
}

func TestSchemaRenameMigration(t *testing.T) {
	ctx := context.Background()
	if SchemaVersion < 2 {
		t.Fatalf("the column rename needs a schema bump, got %d", SchemaVersion)
	}
	path := filepath.Join(t.TempDir(), "data", "old.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	c := conv(acctA, "c1", "Chat", "Pat")
	c.ReadHorizonAt, c.ReadHorizonClientMessageID = base, "cm1"
	must(s.ApplyConversations(ctx, []teamsdesktop.Conversation{c}))
	// Put the archive back into its version 1 shape.
	for _, q := range []string{
		`alter table conversations rename column read_horizon_client_message_id to read_horizon_message_id`,
		`update schema_migrations set version = 1`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening a version 1 archive: %v", err)
	}
	defer func() { _ = s.Close() }()
	var id string
	if err := s.db.QueryRowContext(ctx, `select read_horizon_client_message_id from conversations`).Scan(&id); err != nil || id != "cm1" {
		t.Fatalf("%q %v", id, err)
	}
	must(s.ApplyConversations(ctx, []teamsdesktop.Conversation{c})) // writes work on the migrated table
	if v := must(s.Status(ctx)).SchemaVersion; v != SchemaVersion {
		t.Fatalf("schema version %d", v)
	}

}
