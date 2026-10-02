// Package store is the SQLite archive: schema, idempotent upserts, full-text search and the
// read queries behind every teamscrawl read command. Rows are partitioned by (tenant_id, user_id)
// so two accounts never mix, and messages that vanish from Teams' cache are kept.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	crawlstore "github.com/openclaw/crawlkit/store"
)

// ErrNoArchive is returned by OpenReadOnly when the archive file does not exist. Read commands
// treat it as an empty archive.
var ErrNoArchive = errors.New("no archive yet")

// timeLayout is fixed width so stored timestamps compare correctly as text.
const timeLayout = "2006-01-02T15:04:05.000Z"

// DefaultLimit applies when a Filter has no Limit.
const DefaultLimit = 50

// Store is an open archive.
type Store struct {
	cs       *crawlstore.Store
	db       *sql.DB
	readOnly bool
	// upgrade caches NeedsUpgrade: 0 unknown, 1 current, 2 needs the migration.
	upgrade int
}

// Counts reports what one Apply call did.
type Counts struct {
	Seen      int `json:"seen"`
	Inserted  int `json:"inserted"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
}

// Open creates or opens the archive at path for writing: parent directory 0700, file 0600.
func Open(ctx context.Context, path string) (*Store, error) {
	path = absPath(path)
	if err := prepareArchiveForWrite(path); err != nil {
		return nil, mapArchiveOpenError(err)
	}
	cs, err := crawlstore.Open(ctx, crawlstore.Options{Path: path, Schema: schemaDDL, SchemaVersion: SchemaVersion})
	if err != nil {
		return nil, err
	}
	if err := finalizeArchiveFile(path); err != nil {
		_ = cs.Close()
		return nil, fmt.Errorf("chmod archive: %w", err)
	}
	st := &Store{cs: cs, db: cs.DB()}
	if err := st.migrate(ctx); err != nil {
		_ = cs.Close()
		return nil, err
	}
	return st, nil
}

// absPath returns path as an absolute path. The SQLite driver takes the path as a URI and reads a
// relative one as a host name, so a relative --db would fail; a path that cannot be made absolute
// is used as given.
func absPath(path string) string {
	if abs, err := absFn(path); err == nil {
		return abs
	}
	return path
}

// absFn is filepath.Abs; tests replace it to force the failure.
var absFn = filepath.Abs

// chmodFile is os.Chmod; tests replace it to force the failure.
var chmodFile = os.Chmod

// migrate upgrades an archive made by an older build. Version 2 renamed
// conversations.read_horizon_message_id to read_horizon_client_message_id, and added
// sync_runs.accounts_json (which run-level rows use to say which accounts they refreshed).
func (s *Store) migrate(ctx context.Context) error {
	var old, has int
	if err := s.db.QueryRowContext(ctx, `select
  (select count(*) from pragma_table_info('conversations') where name='read_horizon_message_id'),
  (select count(*) from pragma_table_info('sync_runs') where name='accounts_json')`).Scan(&old, &has); err != nil {
		return err
	}
	if old != 0 {
		if _, err := s.db.ExecContext(ctx, `alter table conversations rename column read_horizon_message_id to read_horizon_client_message_id`); err != nil {
			return err
		}
	}
	if has == 0 {
		if _, err := s.db.ExecContext(ctx, `alter table sync_runs add column accounts_json text`); err != nil {
			return err
		}
		// Runs recorded before run-level rows existed counted for every account; keep them that way.
		_, err := s.db.ExecContext(ctx, `update sync_runs set accounts_json = '["*"]' where status in `+successStatuses)
		return err
	}
	return nil
}

// NeedsUpgrade reports an archive written before run-level sync_runs rows existed: it has no
// sync_runs.accounts_json column, and only a writable open (the next sync) adds it. Reads work on
// such an archive and treat it as having no complete sync yet. The answer is cached per open.
func (s *Store) NeedsUpgrade(ctx context.Context) (bool, error) {
	if s.upgrade == 0 {
		var cols, has int
		if err := s.db.QueryRowContext(ctx, `select count(*), count(case when name='accounts_json' then 1 end) from pragma_table_info('sync_runs')`).Scan(&cols, &has); err != nil {
			return false, err
		}
		if cols == 0 {
			return false, errors.New("no such table: sync_runs")
		}
		s.upgrade = 1
		if has == 0 {
			s.upgrade = 2
		}
	}
	return s.upgrade == 2, nil
}

// OpenReadOnly opens an existing archive read-only (safe beside an active writer). It returns
// ErrNoArchive when the file is missing.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	path = absPath(path)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoArchive
	}
	cs, err := crawlstore.OpenReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	return &Store{cs: cs, db: cs.DB(), readOnly: true}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.cs.Close() }

// ensureParent creates the archive directory 0700. crawlkit would create it 0755, so this runs
// first. The default ~/.teamscrawl is tightened to 0700; a custom --db parent is left alone.
func ensureParent(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(parent) == filepath.Join(home, ".teamscrawl") {
		if err := chmodFile(parent, 0o700); err != nil { //nolint:gosec // G302: a directory, 0700 is the point
			return fmt.Errorf("chmod archive dir: %w", err)
		}
	}
	return nil
}

func fmtTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timeLayout)
}

func parseTime(s sql.NullString) time.Time {
	if !s.Valid || s.String == "" {
		return time.Time{}
	}
	t, err := time.Parse(timeLayout, s.String)
	if err != nil {
		return time.Time{}
	}
	return t
}

// jsonOrNil marshals a non-empty slice; empty ones are stored as NULL.
func jsonOrNil[T any](v []T) any {
	if len(v) == 0 {
		return nil
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func unmarshalNull[T any](s sql.NullString) []T {
	if !s.Valid || s.String == "" {
		return nil
	}
	var out []T
	_ = json.Unmarshal([]byte(s.String), &out)
	return out
}

func rawOrNil(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// SQL runs a read-only query and streams its rows, stopping once limit rows are read; truncated
// says whether more rows existed. It is only available on a store opened with OpenReadOnly. The
// statement check (CheckSQL) is a friendly early error; the connection is also read-only at the
// file level, which is what actually stops writes.
func (s *Store) SQL(ctx context.Context, q string, limit int) (cols []string, out [][]any, truncated bool, err error) {
	if !s.readOnly {
		return nil, nil, false, errors.New("sql requires a read-only archive connection")
	}
	if err := CheckSQL(q); err != nil {
		return nil, nil, false, err
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = rows.Close() }()
	cols, _ = rows.Columns() // fails only on closed rows
	for rows.Next() {
		if len(out) == limit {
			if err := rows.Close(); err != nil {
				return nil, nil, false, err
			}
			return cols, out, true, nil
		}
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		_ = rows.Scan(ptrs...) // scanning into *any cannot fail
		for i, v := range values {
			if b, ok := v.([]byte); ok {
				values[i] = string(b)
			}
		}
		out = append(out, values)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	return cols, out, false, nil
}

// Run is one sync attempt, recorded in sync_runs.
type Run struct {
	StartedAt, FinishedAt time.Time
	Source, Fingerprint   string
	Status                string // ok, ok_with_omissions, unchanged, partial, failed
	Counts                any    // marshaled as counts_json
	Omissions             map[string]int
	// Accounts marks a run-level row (Source ""): the accounts the whole run covered, as
	// "<tenantId>/<userId>", or "*" for every account. Per-source rows leave it nil.
	Accounts []string
}

const successStatuses = `('ok','ok_with_omissions','unchanged')`

// RecordRun appends a sync attempt.
func (s *Store) RecordRun(ctx context.Context, r Run) error { return recordRun(ctx, s.db, r) }

func recordRun(ctx context.Context, db execer, r Run) error {
	var counts, omissions any
	if r.Counts != nil {
		b, err := json.Marshal(r.Counts)
		if err != nil {
			return err
		}
		counts = string(b)
	}
	if len(r.Omissions) > 0 {
		b, _ := json.Marshal(r.Omissions)
		omissions = string(b)
	}
	var accounts any
	if r.Accounts != nil {
		b, _ := json.Marshal(r.Accounts)
		accounts = string(b)
	}
	_, err := db.ExecContext(ctx, `insert into sync_runs(started_at, finished_at, source, fingerprint, status, counts_json, omissions_json, accounts_json) values(?,?,?,?,?,?,?,?)`,
		fmtTime(r.StartedAt), fmtTime(r.FinishedAt), r.Source, r.Fingerprint, r.Status, counts, omissions, accounts)
	return err
}

// LastFingerprint is the fingerprint of the newest successful run for source, or "".
func (s *Store) LastFingerprint(ctx context.Context, source string) (string, error) {
	var fp string
	err := s.db.QueryRowContext(ctx, `select fingerprint from sync_runs where source = ? and status in `+successStatuses+` order by id desc limit 1`, source).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return fp, err
}

// RunRow is a recorded sync attempt.
type RunRow struct {
	ID          int64     `json:"id"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
	Source      string    `json:"source"`
	Fingerprint string    `json:"fingerprint"`
	Status      string    `json:"status"`
	Counts      any       `json:"counts,omitempty"`
	Omissions   any       `json:"omissions,omitempty"`
}

// AccountStatus is one account's archive size.
type AccountStatus struct {
	TenantID      string    `json:"tenant_id"`
	UserID        string    `json:"user_id"`
	Conversations int       `json:"conversations"`
	Messages      int       `json:"messages"`
	People        int       `json:"people"`
	Activity      int       `json:"activity"`
	NewestSentAt  time.Time `json:"newest_sent_at,omitzero"`
	LastSyncedAt  time.Time `json:"last_synced_at,omitzero"`
}

// StatusRow is what `status` and `doctor` report about the archive.
type StatusRow struct {
	SchemaVersion int             `json:"schema_version"`
	FTSPresent    bool            `json:"fts_present"`
	Accounts      []AccountStatus `json:"accounts"`
	NewestSentAt  time.Time       `json:"newest_sent_at,omitzero"`
	LastRun       *RunRow         `json:"last_run,omitempty"`
	LastSuccessAt time.Time       `json:"last_success_at,omitzero"`
}

// Status summarizes the archive.
func (s *Store) Status(ctx context.Context) (StatusRow, error) {
	var st StatusRow
	var err error
	if st.SchemaVersion, err = s.cs.SchemaVersion(ctx); err != nil {
		return st, err
	}
	var fts int
	if err = s.db.QueryRowContext(ctx, `select count(*) from sqlite_master where name in ('message_fts','conversation_fts')`).Scan(&fts); err != nil {
		return st, err
	}
	st.FTSPresent = fts == 2
	st.Accounts = []AccountStatus{}
	rows, err := s.db.QueryContext(ctx, `
select a.tenant_id, a.user_id, a.last_synced_at,
  (select count(*) from conversations c where c.tenant_id=a.tenant_id and c.user_id=a.user_id),
  (select count(*) from messages m where m.tenant_id=a.tenant_id and m.user_id=a.user_id),
  (select count(*) from people p where p.tenant_id=a.tenant_id),
  (select count(*) from activity x where x.tenant_id=a.tenant_id and x.user_id=a.user_id),
  (select max(m.sent_at) from messages m where m.tenant_id=a.tenant_id and m.user_id=a.user_id)
from accounts a order by a.tenant_id, a.user_id`)
	if err != nil {
		return st, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a AccountStatus
		var synced, newest sql.NullString
		if err := rows.Scan(&a.TenantID, &a.UserID, &synced, &a.Conversations, &a.Messages, &a.People, &a.Activity, &newest); err != nil {
			return st, err
		}
		a.LastSyncedAt, a.NewestSentAt = parseTime(synced), parseTime(newest)
		if a.NewestSentAt.After(st.NewestSentAt) {
			st.NewestSentAt = a.NewestSentAt
		}
		st.Accounts = append(st.Accounts, a)
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	var id int64
	var started, finished sql.NullString
	var r RunRow
	var counts, omissions sql.NullString
	err = s.db.QueryRowContext(ctx, `select id, started_at, finished_at, source, fingerprint, status, counts_json, omissions_json from sync_runs order by id desc limit 1`).
		Scan(&id, &started, &finished, &r.Source, &r.Fingerprint, &r.Status, &counts, &omissions)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return st, err
	default:
		r.ID, r.StartedAt, r.FinishedAt = id, parseTime(started), parseTime(finished)
		if counts.Valid {
			_ = json.Unmarshal([]byte(counts.String), &r.Counts)
		}
		if omissions.Valid {
			_ = json.Unmarshal([]byte(omissions.String), &r.Omissions)
		}
		st.LastRun = &r
	}
	if st.LastSuccessAt, err = s.LastSuccess(ctx); err != nil {
		return st, err
	}
	return st, nil
}
