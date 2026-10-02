package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/openclaw/crawlkit/output"

	"github.com/ourostack/teamscrawl/internal/errs"
	"github.com/ourostack/teamscrawl/internal/store"
	"github.com/ourostack/teamscrawl/internal/syncer"
	"github.com/ourostack/teamscrawl/internal/teamsdesktop"
)

const defaultMaxAge = 15 * time.Minute

// runtime is what commands share: the validated globals, the output streams and the clock.
type runtime struct {
	ctx            context.Context
	g              *Globals
	stdout, stderr io.Writer
	stdoutTTY      bool
	stderrTTY      bool
	color          bool // ANSI color in text output
	format         output.Format
	cmd            string // the running command, for command-specific fixes
	maxAge         time.Duration
	fields         []string
	dbPath         string
	root           string
	account        *teamsdesktop.Account
	now            func() time.Time
	exitErr        error       // a failure while printing for a flag that ends the run (--version)
	team           string      // --team of the running read command, validated before any implicit sync
	synced         *syncedInfo // the implicit sync this read ran, if any
}

func newRuntime(ctx context.Context, g *Globals, stdout, stderr io.Writer) *runtime {
	return &runtime{ctx: ctx, g: g, stdout: stdout, stderr: stderr, stdoutTTY: isTTY(stdout), stderrTTY: isTTY(stderr), now: time.Now}
}

// setup validates the global flags and resolves their defaults.
func (rt *runtime) setup() error {
	g := rt.g
	// Validate --format even when --json wins over it.
	format, err := output.Resolve(g.Format, false)
	if err != nil {
		return errs.Usage(err.Error() + " (use text, json or log)")
	}
	if g.JSON || (g.Format == "" && !rt.stdoutTTY) {
		format = output.JSON
	}
	rt.format = format
	rt.color = rt.colorEnabled()
	maxAge := g.MaxAge
	if strings.TrimSpace(maxAge) == "" {
		rt.maxAge = defaultMaxAge
	} else if rt.maxAge, err = parseMaxAge(maxAge); err != nil {
		return errs.Usage(err.Error())
	}
	rt.fields = splitFields(g.Fields)
	if g.MaxText < 0 {
		return errs.Usage("--max-text must be 0 or more")
	}
	rt.dbPath = g.DB
	if rt.dbPath == "" {
		var err error
		rt.dbPath, err = defaultArchivePath()
		if err != nil {
			return err
		}
	}
	rt.root = g.TeamsRoot
	if g.Account != "" {
		if rt.account, err = parseAccount(g.Account); err != nil {
			return err
		}
	}
	return nil
}

func parseAccount(s string) (*teamsdesktop.Account, error) {
	tenant, user, ok := strings.Cut(s, "/")
	user = strings.TrimPrefix(user, "8:orgid:")
	if !ok || tenant == "" || user == "" || strings.Contains(user, "/") {
		return nil, errs.Usage("--account must look like <tenantId>/<userId>; `teamscrawl whoami` lists them")
	}
	return &teamsdesktop.Account{TenantID: tenant, UserID: user}, nil
}

// progress is where sync progress lines go: stderr, and only when it is a terminal.
func (rt *runtime) progress() io.Writer {
	if rt.stderrTTY {
		return rt.stderr
	}
	return nil
}

// ensureFresh runs a sync first when the archive has never synced or its last successful sync is
// older than --max-age. A failed sync does not stop the read: it comes back as a warning. Only
// cancellation is returned as an error.
func (rt *runtime) ensureFresh() (*syncError, error) {
	if rt.maxAge <= 0 {
		return nil, nil
	}
	last, err := rt.lastSuccess()
	if err != nil {
		return nil, err
	}
	if !last.IsZero() && rt.now().Sub(last) <= rt.maxAge {
		return nil, nil
	}
	age := time.Duration(0) // zero: no complete sync yet
	if !last.IsZero() {
		age = rt.now().Sub(last)
	}
	rt.printSyncNotice(age) // stderr only; stdout stays the result
	began := rt.now()
	// The implicit sync always covers every account, so --account can never hide data from a later run.
	rep, _, err := runSync(rt.ctx, syncer.Options{Root: rt.root, DBPath: rt.dbPath, Progress: rt.progress()})
	rt.synced = &syncedInfo{Seconds: math.Round(rt.now().Sub(began).Seconds()*10) / 10, Status: rep.Status}
	if err == nil {
		return nil, nil
	}
	if rt.synced.Status == "" {
		rt.synced.Status = syncer.StatusFailed
	}
	if rt.ctx.Err() != nil {
		return nil, rt.ctx.Err()
	}
	var coded *errs.Coded
	if !errors.As(err, &coded) {
		coded = errs.Internal(err)
	}
	rt.printWarning(coded)
	b := bodyOf(coded)
	return &syncError{Code: b.Code, Message: b.Message}, nil
}

func (rt *runtime) lastSuccess() (time.Time, error) {
	st, err := store.OpenReadOnly(rt.ctx, rt.dbPath)
	if errors.Is(err, store.ErrNoArchive) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, errs.DBError(err)
	}
	defer func() { _ = st.Close() }()
	return rt.freshness(st)
}

// freshness is when the accounts this read covers were last fully synced: --account's own time, or
// the stalest account's for a read of every account. A partial or failed sync does not count.
func (rt *runtime) freshness(st *store.Store) (time.Time, error) {
	var t time.Time
	var err error
	if a := rt.account; a != nil {
		t, err = st.LastSuccessFor(rt.ctx, a.TenantID+"/"+a.UserID)
	} else {
		t, err = st.LastSuccess(rt.ctx)
	}
	if err != nil {
		return time.Time{}, errs.DBError(err)
	}
	return t, nil
}

// checkTeam rejects an unknown or ambiguous --team before the implicit sync spends time on it. An
// archive that does not exist yet cannot say, so the check is repeated after the sync.
func (rt *runtime) checkTeam() error {
	if rt.team == "" {
		return nil
	}
	st, err := store.OpenReadOnly(rt.ctx, rt.dbPath)
	if errors.Is(err, store.ErrNoArchive) {
		return nil
	}
	if err != nil {
		return errs.DBError(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.CheckTeam(rt.ctx, rt.account, rt.team); err != nil {
		return asCoded(err)
	}
	return nil
}

// read runs a read command: implicit sync, open the archive read-only (nil when there is none),
// run fn, stamp the result with the archive's age and any sync error, and print it.
func (rt *runtime) read(label string, fn func(st *store.Store) (result, error)) error {
	if err := rt.checkTeam(); err != nil {
		return err
	}
	syncErr, err := rt.ensureFresh()
	if err != nil {
		return err
	}
	st, err := store.OpenReadOnly(rt.ctx, rt.dbPath)
	switch {
	case errors.Is(err, store.ErrNoArchive):
		if rt.team != "" {
			return store.NoTeam(rt.team)
		}
		st = nil
	case err != nil:
		return errs.DBError(err)
	default:
		defer func() { _ = st.Close() }()
	}
	var age *int64
	if st != nil {
		last, err := rt.freshness(st)
		if err != nil {
			return err
		}
		if !last.IsZero() {
			s := max(int64(rt.now().Sub(last).Seconds()), 0)
			age = &s
		}
	}
	const syncHint = "run teamscrawl sync"
	if age == nil && rt.format == output.Text {
		rt.hint(syncHint) // JSON mode carries the same hint in the result (needs_sync, hint)
	}
	res, err := fn(st)
	if err != nil {
		return asCoded(err)
	}
	res.setMeta(age, syncErr)
	res.setSynced(rt.synced)
	if age == nil {
		res.setNeedsSync(syncHint)
	}
	return rt.write(label, res)
}

// asCoded leaves coded and cancellation errors alone and wraps everything else as an archive error.
func asCoded(err error) error {
	var coded *errs.Coded
	if errors.As(err, &coded) || errors.Is(err, context.Canceled) {
		return err
	}
	return errs.DBError(err)
}

// since parses an optional --since/--until value.
func (rt *runtime) when(flag, val string) (time.Time, error) {
	if val == "" {
		return time.Time{}, nil
	}
	t, err := parseWhen(val, rt.now(), time.Local)
	if err != nil {
		return time.Time{}, errs.Usage(flag + ": " + err.Error())
	}
	return t, nil
}

func checkLimit(n int) error {
	if n < 1 {
		return errs.Usage("--limit must be at least 1")
	}
	return nil
}

// syncNotice is the line a read prints to stderr before its implicit sync, so a wait of several
// seconds is not silent. An age of zero means the archive has no complete sync yet.
func syncNotice(age, maxAge time.Duration) string {
	why := "no complete sync yet"
	if age > 0 {
		if age >= time.Minute {
			age = age.Truncate(time.Minute)
		} else {
			age = age.Truncate(time.Second)
		}
		why = "archive is " + compactDuration(age) + " old"
	}
	return "teamscrawl: syncing — " + why + " (max-age " + compactDuration(maxAge) + ")"
}

// compactDuration is d without the zero parts Go's String keeps: 15m, 2h14m, 1m30s.
func compactDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// syncNoticeDoc is the JSON form of the notice: one line on stderr in json and log modes.
type syncNoticeDoc struct {
	Notice            string `json:"notice"`
	Reason            string `json:"reason"` // stale or never_synced
	ArchiveAgeSeconds *int64 `json:"archive_age_seconds,omitempty"`
	MaxAgeSeconds     int64  `json:"max_age_seconds"`
}

// printSyncNotice tells stderr an implicit sync is starting: a plain line in text mode, one JSON
// line otherwise so every stderr line of a JSON run stays machine-readable. age is zero when the
// archive has no complete sync yet.
func (rt *runtime) printSyncNotice(age time.Duration) {
	if rt.format == output.Text {
		_, _ = fmt.Fprintln(rt.stderr, syncNotice(age, rt.maxAge))
		return
	}
	doc := syncNoticeDoc{Notice: "syncing", Reason: "never_synced", MaxAgeSeconds: int64(rt.maxAge.Seconds())}
	if age > 0 {
		secs := int64(age.Seconds())
		doc.Reason, doc.ArchiveAgeSeconds = "stale", &secs
	}
	rt.writeJSONLine(doc)
}
