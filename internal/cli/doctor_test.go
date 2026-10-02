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
	"github.com/ourostack/teamscrawl/internal/teamsdesktop"
)

func checks(t *testing.T, m map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, c := range m["checks"].([]any) {
		cm := c.(map[string]any)
		out[cm["name"].(string)] = cm
	}
	return out
}

var doctorNames = []string{"teams_installed", "full_disk_access", "teams_origin", "database_writable", "schema_version", "fts", "last_sync_age"}

func TestDoctorAllPass(t *testing.T) {
	e := newEnv(t)
	e.sync()
	code, stdout, stderr := e.run("doctor")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, stdout, stderr)
	}
	m := decode(t, stdout)
	cs := checks(t, m)
	for _, n := range doctorNames {
		c, ok := cs[n]
		if !ok {
			t.Fatalf("missing check %q", n)
		}
		if c["ok"] != true {
			t.Errorf("%s failed: %v", n, c)
		}
		for _, k := range []string{"name", "ok", "detail", "fix"} {
			if _, has := c[k]; !has {
				t.Errorf("%s lacks %q", n, k)
			}
		}
	}
	if m["ok"] != true {
		t.Fatalf("ok = %v", m["ok"])
	}
}

func TestDoctorBeforeFirstSyncPassesWithWarning(t *testing.T) {
	e := newEnv(t)
	code, stdout, stderr := e.run("doctor")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	c := checks(t, decode(t, stdout))["last_sync_age"]
	if c["ok"] != true || c["warn"] != true || c["fix"] == "" {
		t.Fatalf("never-synced must warn, not fail: %v", c)
	}
	if _, err := os.Stat(e.db); err == nil {
		t.Fatal("doctor must not create the archive")
	}
}

func TestDoctorFailExit3(t *testing.T) {
	e := newEnv(t)
	e.root = filepath.Join(t.TempDir(), "absent")
	code, stdout, stderr := e.run("doctor", "--json")
	if code != 3 {
		t.Fatalf("exit %d", code)
	}
	m := decode(t, stdout)
	if m["ok"] != false {
		t.Fatalf("ok = %v", m["ok"])
	}
	c := checks(t, m)["teams_installed"]
	if c["ok"] != false || c["fix"] == "" {
		t.Fatalf("teams_installed = %v", c)
	}
	if er := errorOf(t, stderr); er["code"] != "doctor_failed" {
		t.Fatalf("error = %v", er)
	}
}

func TestDoctorNoFDA(t *testing.T) {
	skipIfRoot(t)
	e := newEnv(t)
	locked := t.TempDir()
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // G302: restoring a directory so TempDir cleanup works
	e.root = locked
	code, stdout, _ := e.run("doctor")
	c := checks(t, decode(t, stdout))["full_disk_access"]
	if code != 3 || c["ok"] != false || c["fix"] == "" {
		t.Fatalf("exit %d, %v", code, c)
	}
}

func doctorChecksFor(t *testing.T, e *env, args ...string) (int, map[string]map[string]any, string) {
	t.Helper()
	code, stdout, stderr := e.run(append([]string{"doctor"}, args...)...)
	return code, checks(t, decode(t, stdout)), stderr
}

func TestDoctorWithoutAnyTeamsOriginFails(t *testing.T) {
	e := newEnv(t)
	e.root = teamsRoot(t, "https_example.com_0")
	code, cs, _ := doctorChecksFor(t, e)
	if code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	if cs["teams_origin"]["ok"] != false || !strings.Contains(cs["teams_origin"]["detail"].(string), "no Teams IndexedDB origin found under "+e.root) || cs["teams_origin"]["fix"] == "" {
		t.Fatalf("teams_origin = %v", cs["teams_origin"])
	}
	if cs["teams_installed"]["ok"] != true || cs["full_disk_access"]["ok"] != true {
		t.Fatalf("an existing root is installed and readable: %v %v", cs["teams_installed"], cs["full_disk_access"])
	}
}

func TestDoctorNamesIgnoredNonTeamsOrigins(t *testing.T) {
	e := newEnv(t)
	e.root = teamsRoot(t, "https_teams.microsoft.com_0", "https_example.com_0")
	code, cs, _ := doctorChecksFor(t, e)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	d := cs["teams_origin"]["detail"].(string)
	if !strings.Contains(d, "1 Teams origin(s) found") || !strings.Contains(d, "ignoring non-Teams origins: https_example.com_0") {
		t.Fatalf("detail = %q", d)
	}
}

func TestDoctorFallsBackToTheDefaultRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "AppData", "Local"))
	t.Setenv("TEAMSCRAWL_DB", filepath.Join(t.TempDir(), "a.db"))
	var out, errb bytes.Buffer
	code := Main([]string{"doctor", "--json"}, &out, &errb)
	if code != 3 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	d := checks(t, decode(t, out.String()))["teams_installed"]["detail"].(string)
	switch goruntime.GOOS {
	case "windows":
		if !strings.Contains(d, `MSTeams_8wekyb3d8bbwe`) {
			t.Fatalf("detail = %q does not name the Windows default root", d)
		}
	default:
		if !strings.Contains(d, "Library/Containers/com.microsoft.teams2") {
			t.Fatalf("detail = %q does not name the default root", d)
		}
	}
}

func TestDoctorFullDiskAccessCheckWindows(t *testing.T) {
	if goruntime.GOOS != "windows" {
		t.Skip("Windows-specific doctor wording")
	}
	e := newEnv(t)
	code, cs, _ := doctorChecksFor(t, e)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	c := cs["full_disk_access"]
	if c["ok"] != true || c["detail"] != "not applicable on Windows" {
		t.Fatalf("full_disk_access = %v", c)
	}
	if fix, _ := c["fix"].(string); fix != "" {
		t.Fatalf("full_disk_access fix = %q, want empty", fix)
	}
}

func TestDoctorRoutesWindowsPermissionDeniedToTeamsOrigin(t *testing.T) {
	if goruntime.GOOS != "windows" {
		t.Skip("Windows-specific doctor wording")
	}
	old := discover
	discover = func(string) ([]teamsdesktop.Source, []string, error) {
		return nil, nil, errs.NoFullDiskAccess(`C:\locked`, errors.New("access denied"))
	}
	t.Cleanup(func() { discover = old })
	e := newEnv(t)
	code, cs, _ := doctorChecksFor(t, e)
	if code != 3 {
		t.Fatalf("exit %d", code)
	}
	fda := cs["full_disk_access"]
	if fda["ok"] != true || fda["detail"] != "not applicable on Windows" {
		t.Fatalf("full_disk_access = %v", fda)
	}
	c := cs["teams_origin"]
	fix := c["fix"].(string)
	if c["ok"] != false || !strings.Contains(c["detail"].(string), `Windows denied access to C:\locked`) || strings.Contains(fix, "Full Disk Access") || !strings.Contains(fix, "Windows account can read") || !strings.Contains(fix, "--teams-root") {
		t.Fatalf("teams_origin = %v", c)
	}
}

func TestDoctorReportsAnArchiveThatCannotBeOpened(t *testing.T) {
	e := newEnv(t)
	e.garbageArchive()
	code, cs, _ := doctorChecksFor(t, e)
	if code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	for _, n := range []string{"schema_version", "fts", "last_sync_age"} {
		if cs[n]["ok"] != false || !strings.Contains(cs[n]["detail"].(string), "cannot open the archive") || !strings.Contains(cs[n]["fix"].(string), "move it aside") {
			t.Errorf("%s = %v", n, cs[n])
		}
	}
}

func TestDoctorReportsAnArchiveThatCannotBeRead(t *testing.T) {
	e := newEnv(t)
	e.sync()
	e.exec("drop table accounts")
	code, cs, _ := doctorChecksFor(t, e)
	if code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	for _, n := range []string{"schema_version", "fts", "last_sync_age"} {
		if cs[n]["ok"] != false || !strings.Contains(cs[n]["detail"].(string), "cannot read the archive") {
			t.Errorf("%s = %v", n, cs[n])
		}
	}
}

func TestDoctorFlagsASchemaVersionMismatchEitherWay(t *testing.T) {
	e := newEnv(t)
	e.sync()
	e.exec("update schema_migrations set version = version + 100")
	code, cs, _ := doctorChecksFor(t, e)
	d := cs["schema_version"]
	if code != 3 || d["ok"] != false || !strings.Contains(d["detail"].(string), "this teamscrawl knows") || d["fix"] != "Update teamscrawl." {
		t.Fatalf("newer archive: exit %d, %v", code, d)
	}
	e.exec("update schema_migrations set version = 1")
	code, cs, _ = doctorChecksFor(t, e)
	d = cs["schema_version"]
	if code != 3 || d["ok"] != false || !strings.Contains(d["detail"].(string), "expected v") || !strings.Contains(d["fix"].(string), "migrate") {
		t.Fatalf("older archive: exit %d, %v", code, d)
	}
}

func TestDoctorFlagsMissingFullTextIndexes(t *testing.T) {
	e := newEnv(t)
	e.sync()
	e.exec("drop table message_fts")
	code, cs, _ := doctorChecksFor(t, e)
	if code != 3 || cs["fts"]["ok"] != false || cs["fts"]["detail"] != "full-text indexes are missing" {
		t.Fatalf("exit %d, fts = %v", code, cs["fts"])
	}
}

func TestDoctorWarnsWhenTheArchiveNeverSucceededOrIsStale(t *testing.T) {
	e := newEnv(t)
	e.sync()
	e.exec("delete from sync_runs")
	code, cs, _ := doctorChecksFor(t, e)
	c := cs["last_sync_age"]
	if code != 0 || c["ok"] != true || c["warn"] != true || c["detail"] != "no successful sync yet" {
		t.Fatalf("empty sync log: exit %d, %v", code, c)
	}
	e.exec(`insert into sync_runs(started_at, finished_at, status, accounts_json) values ('2020-01-01T00:00:00.000Z', '2020-01-01T00:00:01.000Z', 'ok', '["*"]')`)
	code, cs, _ = doctorChecksFor(t, e)
	c = cs["last_sync_age"]
	if code != 0 || c["ok"] != true || c["warn"] != true || !strings.HasPrefix(c["detail"].(string), "last successful sync ") {
		t.Fatalf("stale archive: exit %d, %v", code, c)
	}
}

func TestDoctorChecksWhetherTheArchivePathIsWritable(t *testing.T) {
	skipIfRoot(t)
	e := newEnv(t)
	e.sync()
	dir := filepath.Dir(e.db)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: restoring a directory so TempDir cleanup works
	if err := os.Chmod(dir, 0o500); err != nil {   //nolint:gosec // G302: the point is a read-only directory
		t.Fatal(err)
	}
	code, cs, _ := doctorChecksFor(t, e)
	c := cs["database_writable"]
	if code != 3 || c["ok"] != false || !strings.Contains(c["detail"].(string), "cannot write in "+dir) {
		t.Fatalf("read-only directory: exit %d, %v", code, c)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory
		t.Fatal(err)
	}
	if err := os.Chmod(e.db, 0o400); err != nil {
		t.Fatal(err)
	}
	code, cs, _ = doctorChecksFor(t, e)
	c = cs["database_writable"]
	if code != 3 || c["ok"] != false || !strings.Contains(c["detail"].(string), "cannot write "+e.db) {
		t.Fatalf("read-only archive: exit %d, %v", code, c)
	}
}

func TestDoctorChecksTheNearestExistingDirectoryForAMissingArchiveDir(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()
	e.db = filepath.Join(base, "not", "yet", "there.db")
	code, cs, _ := doctorChecksFor(t, e)
	c := cs["database_writable"]
	if code != 0 || c["ok"] != true || !strings.Contains(c["detail"].(string), base+" is writable") {
		t.Fatalf("exit %d, %v", code, c)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

func TestDoctorReturnsAnOutputWriteFailure(t *testing.T) {
	e := newEnv(t)
	rt := newRuntime(context.Background(), &Globals{}, failingWriter{}, &bytes.Buffer{})
	rt.format = output.JSON
	rt.root, rt.dbPath = e.root, e.db
	if err := (doctorCmd{}).Run(rt); err == nil || !strings.Contains(err.Error(), "pipe closed") {
		t.Fatalf("err = %v, want the write failure", err)
	}
}

func TestDoctorTextShowsFailuresAndWarnings(t *testing.T) {
	e := newEnv(t)
	e.root = filepath.Join(t.TempDir(), "absent")
	code, stdout, _ := e.run("doctor", "--format", "text", "--no-color")
	if code != 3 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "✖") || !strings.Contains(stdout, "▲") {
		t.Fatalf("text doctor lacks failure and warning glyphs:\n%s", stdout)
	}
}

func TestDoctorSnapshotIsOmittedWhenTheArchiveCannotBeRead(t *testing.T) {
	e := newEnv(t)
	e.sync()
	e.exec("drop table accounts")
	rt := newRuntime(context.Background(), &Globals{}, &bytes.Buffer{}, &bytes.Buffer{})
	rt.dbPath = e.db
	if snap := rt.doctorSnapshot(); snap != nil {
		t.Fatalf("snapshot = %+v, want nil", snap)
	}
	rt.dbPath = filepath.Join(t.TempDir(), "none.db")
	if snap := rt.doctorSnapshot(); snap != nil {
		t.Fatalf("snapshot of a missing archive = %+v, want nil", snap)
	}
}
