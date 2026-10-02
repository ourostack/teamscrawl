package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const fixtureRoot = "../../testdata/teams-fixture/EBWebView"

const (
	tenantA = "00000000-0000-4000-8000-000000000001"
	userA   = "00000000-0000-4000-8000-0000000000a1"
)

// env is one isolated CLI invocation environment: a fixture Teams root and a temp archive.
type env struct {
	t    *testing.T
	root string
	db   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("TEAMSCRAWL_MAX_AGE", "")
	t.Setenv("TEAMSCRAWL_DB", "")
	t.Setenv("TEAMSCRAWL_TEAMS_ROOT", "")
	root, err := filepath.Abs(fixtureRoot)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, root: root, db: filepath.Join(t.TempDir(), "data", "teamscrawl.db")}
}

// run invokes Main in-process with the env's root and db; stdout is a buffer, so it is not a TTY.
func (e *env) run(args ...string) (code int, stdout, stderr string) {
	e.t.Helper()
	full := append([]string{"--db", e.db, "--teams-root", e.root}, args...)
	var out, errb bytes.Buffer
	code = Main(full, &out, &errb)
	return code, out.String(), errb.String()
}

func (e *env) sync() {
	e.t.Helper()
	if code, _, stderr := e.run("sync"); code != 0 {
		e.t.Fatalf("sync exit %d: %s", code, stderr)
	}
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, s)
	}
	if dec.More() {
		t.Fatalf("stdout holds more than one JSON document:\n%s", s)
	}
	return m
}

func items(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["items"].([]any)
	if !ok {
		t.Fatalf("no items array in %v", m)
	}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i] = r.(map[string]any)
	}
	return out
}

func errorOf(t *testing.T, stderr string) map[string]any {
	t.Helper()
	var doc struct {
		Error map[string]any `json:"error"`
	}
	for _, line := range bytes.Split([]byte(stderr), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 || line[0] != '{' {
			continue
		}
		if err := json.Unmarshal(line, &doc); err == nil && doc.Error != nil {
			return doc.Error
		}
	}
	t.Fatalf("no JSON error on stderr: %q", stderr)
	return nil
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if !supportsPermissionDeniedSimulation() {
		t.Skip("permission-denied chmod test is not supported on this platform")
	}
	if runningAsPrivilegedUser() {
		t.Skip("permission bits do not restrict the current user")
	}
}

// exec runs SQL directly against the env's archive, to damage it on purpose.
func (e *env) exec(q string) {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.db+"?_pragma=busy_timeout(5000)")
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(q); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

// teamsRoot builds a synthetic EBWebView root holding one profile with the given IndexedDB origins.
func teamsRoot(t *testing.T, origins ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "EBWebView")
	for _, o := range origins {
		if err := os.MkdirAll(filepath.Join(root, "WV2Profile_x", "IndexedDB", o+".indexeddb.leveldb"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if len(origins) == 0 {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
