package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/openclaw/crawlkit/output"

	"github.com/ourostack/teamscrawl/internal/errs"
	"github.com/ourostack/teamscrawl/internal/syncer"
)

// garbageArchive writes a file that is not a SQLite database where the env's archive lives.
func (e *env) garbageArchive() {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.db), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.db, bytes.Repeat([]byte("this is not a database. "), 400), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func setupRuntime(t *testing.T, g Globals, tty bool) (*runtime, error) {
	t.Helper()
	rt := newRuntime(context.Background(), &g, &bytes.Buffer{}, &bytes.Buffer{})
	rt.stdoutTTY = tty
	return rt, rt.setup()
}

func TestSetupDefaultsToTextOnATerminalAndJSONOtherwise(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	rt, err := setupRuntime(t, Globals{DB: "x.db"}, true)
	if err != nil || rt.format != output.Text {
		t.Fatalf("tty: format = %q, err = %v", rt.format, err)
	}
	rt, err = setupRuntime(t, Globals{DB: "x.db"}, false)
	if err != nil || rt.format != output.JSON {
		t.Fatalf("pipe: format = %q, err = %v", rt.format, err)
	}
}

func TestSetupRejectsAnUnknownFormatEvenWithJSON(t *testing.T) {
	rt, err := setupRuntime(t, Globals{DB: "x.db", Format: "text", JSON: true}, false)
	if err != nil || rt.format != output.JSON {
		t.Fatalf("--json must win over --format text: %q, %v", rt.format, err)
	}
	_, err = setupRuntime(t, Globals{DB: "x.db", Format: "yaml", JSON: true}, false)
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != errs.CodeUsage || !strings.Contains(c.Message, "use text, json or log") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupRejectsANegativeMaxText(t *testing.T) {
	_, err := setupRuntime(t, Globals{DB: "x.db", MaxText: -1}, false)
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != errs.CodeUsage || !strings.Contains(c.Message, "--max-text") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupNeedsAHomeDirectoryWhenNoDatabaseIsGiven(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	t.Setenv("LOCALAPPDATA", "")
	_, err := setupRuntime(t, Globals{}, false)
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != errs.CodeUsage || !strings.Contains(c.Message, "pass --db") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("HOME", filepath.Join(string(filepath.Separator), "home", "someone"))
	t.Setenv("USERPROFILE", filepath.Join(string(filepath.Separator), "Users", "someone"))
	t.Setenv("LOCALAPPDATA", filepath.Join(string(filepath.Separator), "Users", "someone", "AppData", "Local"))
	rt, err := setupRuntime(t, Globals{}, false)
	want := filepath.Join(string(filepath.Separator), "home", "someone", ".teamscrawl", "teamscrawl.db")
	if goruntime.GOOS == "windows" {
		want = filepath.Join(string(filepath.Separator), "Users", "someone", "AppData", "Local", "teamscrawl", "teamscrawl.db")
	}
	if err != nil || rt.dbPath != want {
		t.Fatalf("dbPath = %q, err = %v", rt.dbPath, err)
	}
}

func TestRuntimeDefaultArchivePath(t *testing.T) {
	t.Setenv("HOME", filepath.Join(string(filepath.Separator), "home", "someone"))
	t.Setenv("USERPROFILE", filepath.Join(string(filepath.Separator), "Users", "someone"))
	t.Setenv("LOCALAPPDATA", filepath.Join(string(filepath.Separator), "Users", "someone", "AppData", "Local"))
	rt, err := setupRuntime(t, Globals{}, false)
	if err != nil {
		t.Fatalf("setupRuntime: %v", err)
	}
	want := filepath.Join(string(filepath.Separator), "home", "someone", ".teamscrawl", "teamscrawl.db")
	if goruntime.GOOS == "windows" {
		want = filepath.Join(string(filepath.Separator), "Users", "someone", "AppData", "Local", "teamscrawl", "teamscrawl.db")
	}
	if rt.dbPath != want {
		t.Fatalf("dbPath = %q, want %q", rt.dbPath, want)
	}
}

func TestSetupRejectsABadAccount(t *testing.T) {
	if _, err := setupRuntime(t, Globals{DB: "x.db", Account: "no-slash"}, false); err == nil {
		t.Fatal("an account without a slash must fail")
	}
	rt, err := setupRuntime(t, Globals{DB: "x.db", Account: tenantA + "/8:orgid:" + userA}, false)
	if err != nil || rt.account == nil || rt.account.UserID != userA {
		t.Fatalf("account = %+v, err = %v", rt.account, err)
	}
}

func TestProgressGoesToStderrOnlyOnATerminal(t *testing.T) {
	var errb bytes.Buffer
	rt := &runtime{stderr: &errb}
	if rt.progress() != nil {
		t.Fatal("no progress writer off a terminal")
	}
	rt.stderrTTY = true
	if rt.progress() != &errb {
		t.Fatal("progress must be stderr on a terminal")
	}
}

func TestAsCodedKeepsCodedAndCancellationErrors(t *testing.T) {
	coded := errs.Usage("x")
	if got := asCoded(coded); !errors.Is(got, coded) {
		t.Errorf("coded error was rewrapped: %v", got)
	}
	if got := asCoded(context.Canceled); !errors.Is(got, context.Canceled) {
		t.Errorf("cancellation was rewrapped: %v", got)
	}
	var c *errs.Coded
	if got := asCoded(errors.New("sqlite is unhappy")); !errors.As(got, &c) || c.Code != errs.CodeDBError {
		t.Errorf("plain error = %v, want a db_error", got)
	}
}

func TestImplicitSyncCancellationEndsTheRead(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt := newRuntime(ctx, &Globals{}, &bytes.Buffer{}, &bytes.Buffer{})
	rt.maxAge, rt.dbPath, rt.root = defaultMaxAge, e.db, e.root
	rt.format = output.JSON
	if _, err := rt.ensureFresh(); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	code, _, stderr := func() (int, string, string) {
		var out, errb bytes.Buffer
		c := runCLI(ctx, []string{"--db", e.db, "--teams-root", e.root, "search", "x"}, &out, &errb)
		return c, out.String(), errb.String()
	}()
	if code != errs.ExitRuntime || errorOf(t, stderr)["code"] != errs.CodeInterrupted {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
}

func TestImplicitSyncUncodedFailureBecomesAnInternalWarning(t *testing.T) {
	old := runSync
	runSync = func(context.Context, syncer.Options) (syncer.Report, []syncer.Change, error) {
		return syncer.Report{}, nil, errors.New("disk on fire")
	}
	t.Cleanup(func() { runSync = old })
	e := newEnv(t)
	code, stdout, stderr := e.run("search", "x")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	se, _ := decode(t, stdout)["sync_error"].(map[string]any)
	if se["code"] != errs.CodeInternal || !strings.Contains(se["message"].(string), "disk on fire") {
		t.Fatalf("sync_error = %v", se)
	}
	if !strings.Contains(stderr, `"warning"`) {
		t.Fatalf("stderr lacks the warning: %s", stderr)
	}
}

func TestReadCommandsReportAnUnreadableArchiveAsDBError(t *testing.T) {
	e := newEnv(t)
	e.garbageArchive()
	for _, args := range [][]string{{"search", "x"}, {"--max-age", "0", "search", "x"}} {
		code, _, stderr := e.run(args...)
		if code != errs.ExitRuntime || errorOf(t, stderr)["code"] != errs.CodeDBError {
			t.Errorf("%v: exit %d, stderr %s", args, code, stderr)
		}
	}
}

func TestReadCommandsReportABrokenSyncLogAsDBError(t *testing.T) {
	e := newEnv(t)
	e.sync()
	e.exec("drop table sync_runs")
	for _, args := range [][]string{{"search", "Fixture"}, {"--max-age", "0", "search", "Fixture"}} {
		code, _, stderr := e.run(args...)
		if code != errs.ExitRuntime || errorOf(t, stderr)["code"] != errs.CodeDBError {
			t.Errorf("%v: exit %d, stderr %s", args, code, stderr)
		}
	}
}
