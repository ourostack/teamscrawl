package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// errInjected is what the fault driver returns, so a test can tell an injected failure from a
// real one.
var errInjected = errors.New("injected fault")

// faultKinds are the places the fault driver can fail. "scan" does not return an error from the
// driver: it hands the row back as all NULLs, which makes the caller's Scan fail.
var faultKinds = []string{"begin", "commit", "prepare", "exec", "query", "next", "scan", "rowsclose", "lastid"}

// injector counts driver calls by kind and fails the at-th call of the armed kind.
type injector struct {
	mu    sync.Mutex
	kind  string
	at    int
	seen  map[string]int
	fired bool
}

func (f *injector) arm(kind string, at int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kind, f.at, f.seen, f.fired = kind, at, map[string]int{}, false
}

func (f *injector) disarm() (fired bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fired, f.kind = f.fired, ""
	return fired
}

func (f *injector) hit(kind string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kind != kind {
		return false
	}
	f.seen[kind]++
	if f.seen[kind] == f.at {
		f.fired = true
		return true
	}
	return false
}

type faultConnector struct {
	dsn string
	inj *injector
}

func (c faultConnector) Connect(context.Context) (driver.Conn, error) {
	inner, err := (&sqliteOpener{}).open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &faultConn{inner: inner, inj: c.inj}, nil
}

func (c faultConnector) Driver() driver.Driver { return nil }

// sqliteOpener opens the real driver connection the fault driver wraps.
type sqliteOpener struct{}

func (*sqliteOpener) open(dsn string) (driver.Conn, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	return db.Driver().Open(dsn)
}

type faultConn struct {
	inner driver.Conn
	inj   *injector
}

func (c *faultConn) Prepare(q string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), q)
}
func (c *faultConn) Close() error { return c.inner.Close() }
func (c *faultConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *faultConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	if c.inj.hit("prepare") {
		return nil, errInjected
	}
	st, err := c.inner.(driver.ConnPrepareContext).PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return &faultStmt{inner: st, inj: c.inj}, nil
}

func (c *faultConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	if c.inj.hit("begin") {
		return nil, errInjected
	}
	tx, err := c.inner.(driver.ConnBeginTx).BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &faultTx{inner: tx, inj: c.inj}, nil
}

func (c *faultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if c.inj.hit("exec") {
		return nil, errInjected
	}
	res, err := c.inner.(driver.ExecerContext).ExecContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	return &faultResult{inner: res, inj: c.inj}, nil
}

func (c *faultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if c.inj.hit("query") {
		return nil, errInjected
	}
	rows, err := c.inner.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	return &faultRows{inner: rows, inj: c.inj}, nil
}

type faultStmt struct {
	inner driver.Stmt
	inj   *injector
}

func (s *faultStmt) Close() error  { return s.inner.Close() }
func (s *faultStmt) NumInput() int { return s.inner.NumInput() }
func (s *faultStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("fault driver: use ExecContext")
}
func (s *faultStmt) Query([]driver.Value) (driver.Rows, error) {
	return nil, errors.New("fault driver: use QueryContext")
}

func (s *faultStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if s.inj.hit("exec") {
		return nil, errInjected
	}
	res, err := s.inner.(driver.StmtExecContext).ExecContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return &faultResult{inner: res, inj: s.inj}, nil
}

func (s *faultStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if s.inj.hit("query") {
		return nil, errInjected
	}
	rows, err := s.inner.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return &faultRows{inner: rows, inj: s.inj}, nil
}

type faultTx struct {
	inner driver.Tx
	inj   *injector
}

func (t *faultTx) Commit() error {
	if t.inj.hit("commit") {
		_ = t.inner.Rollback()
		return errInjected
	}
	return t.inner.Commit()
}
func (t *faultTx) Rollback() error { return t.inner.Rollback() }

type faultResult struct {
	inner driver.Result
	inj   *injector
}

func (r *faultResult) LastInsertId() (int64, error) {
	if r.inj.hit("lastid") {
		return 0, errInjected
	}
	return r.inner.LastInsertId()
}
func (r *faultResult) RowsAffected() (int64, error) { return r.inner.RowsAffected() }

type faultRows struct {
	inner driver.Rows
	inj   *injector
}

func (r *faultRows) Columns() []string { return r.inner.Columns() }
func (r *faultRows) Close() error {
	err := r.inner.Close()
	if r.inj.hit("rowsclose") {
		return errInjected
	}
	return err
}

func (r *faultRows) Next(dest []driver.Value) error {
	if r.inj.hit("next") {
		return errInjected
	}
	if err := r.inner.Next(dest); err != nil {
		return err
	}
	if r.inj.hit("scan") {
		for i := range dest {
			dest[i] = nil
		}
	}
	return nil
}

// injectFaults points s's write and query connection at the same file through the fault driver
// and returns the injector. Direct reads of the file (dump) keep using the real connection.
func injectFaults(t *testing.T, s *Store) *injector {
	t.Helper()
	inj := &injector{seen: map[string]int{}}
	path := s.cs.Path()
	if runtime.GOOS == "windows" {
		path = filepath.ToSlash(path)
		if filepath.VolumeName(path) != "" && !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
	}
	db := sql.OpenDB(faultConnector{dsn: (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=busy_timeout(5000)", inj: inj})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s.db = db
	return inj
}

// dump renders every table, index tables included, so a test can prove a failed call left the
// archive exactly as it was.
func dump(t *testing.T, s *Store) string {
	t.Helper()
	var b strings.Builder
	for _, tbl := range []string{"accounts", "conversations", "messages", "people", "activity", "sync_runs", "meta", "message_fts", "conversation_fts"} {
		rows, err := s.cs.DB().Query(`select rowid, * from ` + tbl + ` order by rowid`)
		if err != nil {
			t.Fatalf("dump %s: %v", tbl, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("dump scan: %v", err)
			}
			fmt.Fprintln(&b, tbl, vals)
		}
		_ = rows.Close()
	}
	return b.String()
}

// sweepFaults fails op once at every driver call it makes, for every fault kind in turn, on a
// fresh archive prepared by setup. Each injected failure must come back as an error from op, and a
// failed op must leave the archive untouched.
//
// nullSafe lists the rows (1-based, in the order the op reads them) whose Scan destinations are
// all nullable, so an all-NULL row is a legitimate result there and not a swallowed failure.
func sweepFaults(t *testing.T, setup func(t *testing.T, s *Store), op func(ctx context.Context, s *Store) error, nullSafe ...int) {
	t.Helper()
	for _, kind := range faultKinds {
		t.Run(kind, func(t *testing.T) {
			s := newStore(t)
			if setup != nil {
				setup(t, s)
			}
			inj := injectFaults(t, s)
			sweepKind(t, s, inj, kind, true, op, nullSafe)
		})
	}
}

// sweepReadFaults is sweepFaults for a read: it shares one archive across the fault kinds a read
// can meet (a read opens no transaction and writes nothing).
func sweepReadFaults(t *testing.T, setup func(t *testing.T, s *Store), op func(ctx context.Context, s *Store) error, nullSafe ...int) {
	t.Helper()
	s := newStore(t)
	setup(t, s)
	inj := injectFaults(t, s)
	for _, kind := range []string{"prepare", "query", "next", "scan", "rowsclose"} {
		sweepKind(t, s, inj, kind, false, op, nullSafe)
	}
}

func sweepKind(t *testing.T, s *Store, inj *injector, kind string, checkState bool, op func(ctx context.Context, s *Store) error, nullSafe []int) {
	t.Helper()
	ctx := context.Background()
	before := ""
	if checkState {
		before = dump(t, s)
	}
	for at := 1; ; at++ {
		if at > 5000 {
			t.Fatalf("%s: sweep did not end", kind)
		}
		inj.arm(kind, at)
		err := op(ctx, s)
		if !inj.disarm() {
			if err != nil {
				t.Fatalf("%s: unfaulted call failed: %v", kind, err)
			}
			return
		}
		if err == nil && kind == "scan" && slices.Contains(nullSafe, at) {
			continue
		}
		if err == nil {
			t.Fatalf("%s #%d: failure was swallowed", kind, at)
		}
		if kind != "scan" && !errors.Is(err, errInjected) {
			t.Fatalf("%s #%d: error lost its cause: %v", kind, at, err)
		}
		if checkState {
			if after := dump(t, s); after != before {
				t.Fatalf("%s #%d: failed call changed the archive:\n%s\n---\n%s", kind, at, before, after)
			}
		}
	}
}
