//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	tenant1 = "00000000-0000-4000-8000-000000000001"
	user1   = "00000000-0000-4000-8000-0000000000a1"
	tenant2 = "00000000-0000-4000-8000-000000000002"
	user2   = "00000000-0000-4000-8000-0000000000a2"

	account1 = tenant1 + "/" + user1
	account2 = tenant2 + "/" + user2
)

// fixtureRoot is the committed fixture. Tests only ever read it through copyFixture.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../testdata/teams-fixture/EBWebView")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// copyFixture copies the committed fixture into the test's own temp dir so a test can change it.
func copyFixture(t *testing.T) string {
	t.Helper()
	src := fixtureRoot(t)
	dst := filepath.Join(t.TempDir(), "EBWebView")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := os.ReadFile(p) //nolint:gosec // G304: walking the committed fixture
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600) //nolint:gosec // G703: target is under the test's temp dir
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

// env is one isolated machine: its own home and temp roots, Teams root and archive.
type env struct {
	t    *testing.T
	home string
	tmp  string
	root string // a private copy of the fixture's EBWebView directory
	db   string // explicit archive path, used unless a test passes its own --db
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{t: t, home: filepath.Join(base, "home"), tmp: filepath.Join(base, "tmp"), root: copyFixture(t)}
	for _, d := range []string{e.home, e.tmp, filepath.Join(e.home, "AppData", "Local")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.db = filepath.Join(base, "archive", "teamscrawl.db")
	return e
}

// baseArgs points a command at this machine's fixture copy and archive.
func (e *env) baseArgs() []string { return []string{"--teams-root", e.root, "--db", e.db} }

func (e *env) localAppData() string { return filepath.Join(e.home, "AppData", "Local") }

func (e *env) defaultArchivePath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(e.localAppData(), "teamscrawl", "teamscrawl.db")
	}
	return filepath.Join(e.home, ".teamscrawl", "teamscrawl.db")
}

// environ is the process environment for a run: inherited, minus anything that would change
// teamscrawl's behavior, plus the isolated home and temp roots. extra entries win.
func (e *env) environ(extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "TEAMSCRAWL_"), k == "NO_COLOR", k == "CLICOLOR", k == "CLICOLOR_FORCE", k == "HOME", k == "TMPDIR", k == "TMP", k == "TEMP", k == "USERPROFILE", k == "LOCALAPPDATA":
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "HOME="+e.home, "TMPDIR="+e.tmp, "TMP="+e.tmp, "TEMP="+e.tmp, "USERPROFILE="+e.home, "LOCALAPPDATA="+e.localAppData())
	return append(out, extra...)
}

type result struct {
	stdout, stderr string
	code           int
}

// runWith runs the binary with extra environment entries and returns what it printed and its exit
// code. Stdout and stderr are pipes, so the binary sees no terminal.
func (e *env) runWith(extra []string, args ...string) result {
	e.t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(binary, args...) //nolint:gosec // G204: binary is the one TestMain built
	cmd.Env = e.environ(extra...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return finish(e.t, cmd, err, stdout.String(), stderr.String())
}

func finish(t *testing.T, cmd *exec.Cmd, err error, stdout, stderr string) result {
	t.Helper()
	res := result{stdout: stdout, stderr: stderr}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.code = ee.ExitCode()
	default:
		t.Fatalf("run %v: %v", cmd.Args, err)
	}
	return res
}

// run is runWith with no extra environment.
func (e *env) run(args ...string) result {
	e.t.Helper()
	return e.runWith(nil, args...)
}

// cmd runs a subcommand against this machine's fixture copy and archive: base args first, then args.
func (e *env) cmd(args ...string) result {
	e.t.Helper()
	return e.run(append(args, e.baseArgs()...)...)
}

// sync runs a successful sync and returns its report.
func (e *env) sync() map[string]any {
	e.t.Helper()
	res := e.cmd("sync")
	mustExit(e.t, res, 0)
	return mustJSON(e.t, res.stdout)
}

func mustExit(t *testing.T, res result, want int) {
	t.Helper()
	if res.code != want {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", res.code, want, res.stdout, res.stderr)
	}
}

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, s)
	}
	return m
}

// ok asserts a clean exit and a quiet stderr, and returns stdout as a JSON object.
func ok(t *testing.T, res result) map[string]any {
	t.Helper()
	mustExit(t, res, 0)
	if res.stderr != "" {
		t.Fatalf("stderr is not empty: %s", res.stderr)
	}
	return mustJSON(t, res.stdout)
}

// list asserts a clean list result and returns its items. count must equal len(items).
func list(t *testing.T, res result) (items []map[string]any, whole map[string]any) {
	t.Helper()
	whole = ok(t, res)
	raw, isList := whole["items"].([]any)
	if !isList {
		t.Fatalf("no items array in %s", res.stdout)
	}
	if n, _ := whole["count"].(float64); int(n) != len(raw) {
		t.Fatalf("count = %v but %d items", whole["count"], len(raw))
	}
	if _, has := whole["truncated"].(bool); !has {
		t.Fatalf("no truncated bool in %s", res.stdout)
	}
	for _, r := range raw {
		m, isMap := r.(map[string]any)
		if !isMap {
			t.Fatalf("item is not an object: %v", r)
		}
		items = append(items, m)
	}
	return items, whole
}

func strs(items []map[string]any, key string) []string {
	var out []string
	for _, it := range items {
		s, _ := it[key].(string)
		out = append(out, s)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// wantError asserts a failed run: empty stdout, the exit code, and one {"error":{code,message,fix}}
// object on stderr with a non-empty message and fix, and no stack trace.
func wantError(t *testing.T, res result, exit int, code string) map[string]any {
	t.Helper()
	mustExit(t, res, exit)
	if res.stdout != "" {
		t.Fatalf("stdout must be empty on an error, got %s", res.stdout)
	}
	body := mustJSON(t, res.stderr)
	e, _ := body["error"].(map[string]any)
	if e == nil || len(body) != 1 {
		t.Fatalf("stderr is not exactly {\"error\":{...}}: %s", res.stderr)
	}
	if e["code"] != code {
		t.Fatalf("error code = %v, want %q\n%s", e["code"], code, res.stderr)
	}
	for _, k := range []string{"message", "fix"} {
		if s, _ := e[k].(string); strings.TrimSpace(s) == "" {
			t.Fatalf("error %s is empty: %s", k, res.stderr)
		}
	}
	if strings.Contains(res.stderr, "goroutine ") || strings.Contains(res.stderr, ".go:") {
		t.Fatalf("stderr looks like a stack trace: %s", res.stderr)
	}
	return e
}

// snapshots lists teamscrawl-snapshot-* directories left in dir.
func snapshots(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "teamscrawl-snapshot-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// started is a running subprocess whose output is captured.
type started struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdout *syncBuffer
	stderr *syncBuffer
	done   chan struct{}
	err    error
}

// start launches the binary in the background. The process is killed when the test ends if the
// test has not already waited for it.
func (e *env) start(extra []string, args ...string) *started {
	e.t.Helper()
	s := &started{t: e.t, stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan struct{})}
	s.cmd = exec.Command(binary, args...) //nolint:gosec // G204: binary is the one TestMain built
	s.cmd.Env = e.environ(extra...)
	s.cmd.Stdout, s.cmd.Stderr = s.stdout, s.stderr
	if err := s.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	go func() { s.err = s.cmd.Wait(); close(s.done) }()
	e.t.Cleanup(func() {
		select {
		case <-s.done:
		default:
			_ = s.cmd.Process.Kill()
			<-s.done
		}
	})
	return s
}

// signal sends sig and waits up to 15 s for the process to exit; it returns the result.
func (s *started) signal(sig syscall.Signal) result {
	s.t.Helper()
	if err := s.cmd.Process.Signal(sig); err != nil {
		s.t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
		s.t.Fatalf("process did not exit after %v\nstdout: %s\nstderr: %s", sig, s.stdout.String(), s.stderr.String())
	}
	return finish(s.t, s.cmd, s.err, s.stdout.String(), s.stderr.String())
}

// waitFor polls cond for up to 15 s and fails with what on timeout. It also fails fast if the
// process exits first.
func (s *started) waitFor(what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		select {
		case <-s.done:
			s.t.Fatalf("process exited while waiting for %s (err %v)\nstdout: %s\nstderr: %s", what, s.err, s.stdout.String(), s.stderr.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	s.t.Fatalf("timed out waiting for %s\nstdout: %s\nstderr: %s", what, s.stdout.String(), s.stderr.String())
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 does not deny access, so this test cannot run")
	}
}

func skipIfWindowsPermissionSimulation(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not provide the chmod-based unreadability this test assumes")
	}
}

func skipIfWindowsSubprocessSignals(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows subprocess signal delivery is not reliable for this e2e scenario")
	}
}

// syncBuffer is a bytes.Buffer that is safe to read while a subprocess writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
