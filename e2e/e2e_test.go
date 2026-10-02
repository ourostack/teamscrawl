//go:build e2e

package e2e

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var binary string

func testBinaryName() string {
	if runtime.GOOS == "windows" {
		return "teamscrawl.exe"
	}
	return "teamscrawl"
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "teamscrawl-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkdir temp:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	binary = filepath.Join(dir, testBinaryName())
	build := exec.Command( //nolint:gosec // fixed arguments; binary path is a temp dir we created
		"go", "build", "-ldflags", "-X github.com/ourostack/teamscrawl/internal/cli.version=e2e", "-o", binary, "../cmd/teamscrawl")
	build.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

func TestVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"--json", "--version"}} {
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(binary, args...) //nolint:gosec // G204: binary is the one TestMain built
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("teamscrawl %v: %v\nstderr: %s", args, err, stderr.String())
		}
		m := mustJSON(t, stdout.String()) // piped, so JSON: exactly one document
		if len(m) != 3 || m["version"] != "e2e" || m["commit"] == "" || m["date"] == "" || stderr.Len() != 0 {
			t.Fatalf("teamscrawl %v: %s (stderr %q)", args, stdout.String(), stderr.String())
		}
	}
	var stdout bytes.Buffer
	cmd := exec.Command(binary, "version", "--format", "text")
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil || !strings.HasPrefix(stdout.String(), "teamscrawl e2e (commit ") {
		t.Fatalf("text version: %v %q", err, stdout.String())
	}
}

// SIGTERM while watch is mid-sync (held after the snapshot) exits 0 and removes the snapshot.
func TestWatchSIGTERM(t *testing.T) {
	skipIfWindowsSubprocessSignals(t)
	tmp := t.TempDir()
	root, err := filepath.Abs("../testdata/teams-fixture/EBWebView")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(binary, "watch", "--every", "1h", "--db", filepath.Join(tmp, "a.db"), "--teams-root", root, "--json") //nolint:gosec // G204: binary is the one this test built
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp, "TEAMSCRAWL_TEST_PAUSE_AFTER_SNAPSHOT=30s")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(tmp, "teamscrawl-snapshot-*")
	deadline := time.Now().Add(15 * time.Second)
	for {
		if m, _ := filepath.Glob(snap); len(m) > 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("no snapshot appeared\nstderr: %s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("watch after SIGTERM: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if m, _ := filepath.Glob(snap); len(m) != 0 {
		t.Fatalf("snapshot left behind: %v", m)
	}
}

// pausedMarker is what the binary prints to stderr when TEAMSCRAWL_TEST_PAUSE_AFTER_SNAPSHOT starts
// its pause: the snapshot is complete and the process is waiting, so a signal lands inside the pause.
const pausedMarker = "teamscrawl-test: paused after snapshot"

const (
	chat1 = "19:00000000-0000-4000-8000-0000000000a1_00000000-0000-4000-8000-0000000000ff@unq.gbl.spaces"
	chat2 = "19:00000000-0000-4000-8000-0000000000a2_00000000-0000-4000-8000-0000000000ff@unq.gbl.spaces"

	channel1 = "19:topicchannel1@thread.tacv2"
)

func num(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	f, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%q is not a number in %v", key, m)
	}
	return int(f)
}

// goldenCount is the number of entries in one of the fixture's golden files; after one sync the
// archive holds exactly that many of each kind, both accounts together.
func goldenCount(t *testing.T, name string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "teams-fixture", "expected", name)) //nolint:gosec // G304: fixed golden file names
	if err != nil {
		t.Fatal(err)
	}
	var v []json.RawMessage
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return len(v)
}

func TestE2EFreshMachine(t *testing.T) {
	e := newEnv(t)
	root := []string{"--teams-root", e.root}

	doc := ok(t, e.run(append([]string{"doctor"}, root...)...))
	if doc["ok"] != true {
		t.Fatalf("doctor not ok: %v", doc)
	}
	checks, _ := doc["checks"].([]any)
	if len(checks) == 0 {
		t.Fatal("doctor printed no checks")
	}
	for _, c := range checks {
		m, _ := c.(map[string]any)
		if m["ok"] != true {
			t.Fatalf("check failed: %v", m)
		}
	}

	// No --db: the archive lands at the default path under the fresh HOME.
	rep := ok(t, e.run(append([]string{"sync"}, root...)...))
	if rep["status"] != "ok" {
		t.Fatalf("sync status = %v", rep["status"])
	}
	if msgs, _ := rep["messages"].(map[string]any); num(t, msgs, "inserted") != goldenCount(t, "mapped-messages.json") {
		t.Fatalf("messages inserted = %v, golden %d", msgs, goldenCount(t, "mapped-messages.json"))
	}

	st := ok(t, e.run(append([]string{"status", "--json"}, root...)...))
	if want := e.defaultArchivePath(); st["archive_path"] != want {
		t.Fatalf("archive_path = %v, want %s", st["archive_path"], want)
	}
	accts, _ := st["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("want 2 accounts, got %v", st["accounts"])
	}
	var convs, msgs, acts int
	for _, a := range accts {
		m, _ := a.(map[string]any)
		if num(t, m, "conversations") != 7 || num(t, m, "messages") != 55 || num(t, m, "people") != 5 || num(t, m, "activity") != 11 {
			t.Fatalf("account counts = %v, want 7/55/5/11", m)
		}
		convs += num(t, m, "conversations")
		msgs += num(t, m, "messages")
		acts += num(t, m, "activity")
	}
	if convs != goldenCount(t, "mapped-conversations.json") || msgs != goldenCount(t, "mapped-messages.json") || acts != goldenCount(t, "mapped-activity.json") {
		t.Fatalf("totals %d/%d/%d do not match the golden files", convs, msgs, acts)
	}

	items, _ := list(t, e.run(append([]string{"search", "Hello from Alex"}, root...)...))
	if len(items) != 1 || items[0]["text"] != "Hello from Alex Fixture" || items[0]["conversation_display_name"] != "Fixture chat 1" {
		t.Fatalf("search items = %v", items)
	}

	items, _ = list(t, e.run(append([]string{"messages", "--conversation", "Fixture chat 1"}, root...)...))
	if len(items) < 10 {
		t.Fatalf("want the chat's messages, got %d", len(items))
	}
	ids := strs(items, "id")
	if !sort.StringsAreSorted(ids) || ids[0] != "1700000001000" {
		t.Fatalf("messages are not oldest first: %v", ids)
	}
	sent := strs(items, "sent_at")
	if !sort.StringsAreSorted(sent) {
		t.Fatalf("sent_at is not ascending: %v", sent)
	}
	for _, it := range items {
		if it["conversation_id"] != chat1 || it["tenant_id"] != tenant1 {
			t.Fatalf("a message from another conversation or account: %v", it)
		}
	}

	items, _ = list(t, e.run(append([]string{"conversations", "--kind", "chat"}, root...)...))
	if len(items) == 0 {
		t.Fatal("no chats")
	}
	for _, it := range items {
		if it["kind"] != "Chat" {
			t.Fatalf("--kind chat returned %v", it["kind"])
		}
	}
	if names := strs(items, "display_name"); !contains(names, "Fixture chat 1") || !contains(names, "Fixture chat 2") {
		t.Fatalf("chats = %v", names)
	}

	items, _ = list(t, e.run(append([]string{"people", "--query", "Pat"}, root...)...))
	if len(items) == 0 {
		t.Fatal("people --query Pat found nobody")
	}
	for _, it := range items {
		if it["display_name"] != "Pat Example" {
			t.Fatalf("person = %v", it)
		}
	}

	res := ok(t, e.run(append([]string{"sql", "select count(*) from messages"}, root...)...))
	if rows, _ := res["rows"].([]any); len(rows) != 1 || rows[0].([]any)[0] != float64(110) {
		t.Fatalf("sql rows = %v", res["rows"])
	}
}

func TestE2EIdempotent(t *testing.T) {
	e := newEnv(t)
	first := e.sync()
	if first["status"] != "ok" {
		t.Fatalf("first sync = %v", first["status"])
	}
	second := e.sync()
	if second["status"] != "unchanged" {
		t.Fatalf("second sync status = %v, want unchanged", second["status"])
	}
	if m, _ := second["messages"].(map[string]any); num(t, m, "inserted") != 0 || num(t, m, "updated") != 0 {
		t.Fatalf("second sync changed messages: %v", m)
	}
	res := ok(t, e.cmd("sql", "select count(*) from messages"))
	if rows, _ := res["rows"].([]any); rows[0].([]any)[0] != float64(110) {
		t.Fatalf("rows after two syncs = %v", res["rows"])
	}
}

func TestE2ETwoAccounts(t *testing.T) {
	e := newEnv(t)
	e.sync()

	who := ok(t, e.cmd("whoami"))
	accts, _ := who["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("whoami accounts = %v", who["accounts"])
	}
	names := map[string]string{}
	for _, a := range accts {
		m, _ := a.(map[string]any)
		names[m["tenant_id"].(string)+"/"+m["user_id"].(string)] = m["display_name"].(string)
	}
	if names[account1] != "Alex Fixture" || names[account2] != "Blair Fixture" {
		t.Fatalf("whoami names = %v", names)
	}

	// The shared conversation id exists once per account and stays partitioned.
	all, _ := list(t, e.cmd("conversations"))
	var shared int
	for _, it := range all {
		if it["id"] == "19:shared-fixture-conversation@thread.v2" {
			shared++
		}
	}
	if shared != 2 {
		t.Fatalf("the shared conversation id should appear once per account, got %d", shared)
	}

	for _, tc := range []struct{ account, tenant, user, ownChat, otherChat string }{
		{account1, tenant1, user1, "Fixture chat 1", "Fixture chat 2"},
		{account2, tenant2, user2, "Fixture chat 2", "Fixture chat 1"},
	} {
		items, _ := list(t, e.cmd("conversations", "--account", tc.account))
		if len(items) != 7 {
			t.Fatalf("%s: %d conversations, want 7", tc.account, len(items))
		}
		for _, it := range items {
			if it["tenant_id"] != tc.tenant || it["user_id"] != tc.user {
				t.Fatalf("--account %s leaked %v", tc.account, it)
			}
		}
		own, _ := list(t, e.cmd("messages", "-c", tc.ownChat, "--account", tc.account))
		if len(own) == 0 {
			t.Fatalf("%s sees none of its own chat", tc.account)
		}
		other, _ := list(t, e.cmd("messages", "-c", tc.otherChat, "--account", tc.account))
		if len(other) != 0 {
			t.Fatalf("%s sees the other account's chat: %v", tc.account, other)
		}
		hits, _ := list(t, e.cmd("search", "Hello", "--account", tc.account))
		if len(hits) != 1 || hits[0]["tenant_id"] != tc.tenant {
			t.Fatalf("%s search = %v", tc.account, hits)
		}
	}
	// The user id may carry the 8:orgid: prefix.
	items, _ := list(t, e.cmd("conversations", "--account", tenant2+"/8:orgid:"+user2))
	if len(items) != 7 {
		t.Fatalf("prefixed account id: %d conversations", len(items))
	}
	wantError(t, e.cmd("conversations", "--account", "nonsense"), 2, "usage")
}

func TestE2EErrors(t *testing.T) {
	t.Run("no full disk access", func(t *testing.T) {
		skipIfRoot(t)
		skipIfWindowsPermissionSimulation(t)
		e := newEnv(t)
		if err := os.Chmod(e.root, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(e.root, 0o700) }) //nolint:gosec // G302: a directory must be searchable
		wantError(t, e.cmd("sync"), 3, "no_full_disk_access")
		wantError(t, e.cmd("sync", "--max-age", "0"), 3, "no_full_disk_access")

		// doctor reports the failing check on stdout and fails with doctor_failed on stderr.
		res := e.cmd("doctor")
		mustExit(t, res, 3)
		doc := mustJSON(t, res.stdout)
		if doc["ok"] != false {
			t.Fatalf("doctor ok = %v", doc["ok"])
		}
		var failing []string
		for _, c := range doc["checks"].([]any) {
			m := c.(map[string]any)
			if m["ok"] == false {
				failing = append(failing, m["name"].(string))
				if m["fix"] == "" {
					t.Fatalf("failing check without a fix: %v", m)
				}
			}
		}
		if !contains(failing, "full_disk_access") {
			t.Fatalf("failing checks = %v", failing)
		}
		wantDoctorFailed(t, res)
	})

	t.Run("teams not installed", func(t *testing.T) {
		e := newEnv(t)
		e.root = filepath.Join(filepath.Dir(e.root), "missing")
		wantError(t, e.cmd("sync"), 3, "teams_not_installed")
		res := e.cmd("doctor")
		mustExit(t, res, 3)
		if doc := mustJSON(t, res.stdout); doc["ok"] != false {
			t.Fatalf("doctor ok = %v", doc["ok"])
		}
		wantDoctorFailed(t, res)
	})

	t.Run("no teams origin", func(t *testing.T) {
		e := newEnv(t)
		empty := filepath.Join(filepath.Dir(e.root), "empty")
		if err := os.MkdirAll(filepath.Join(empty, "WV2Profile_x", "IndexedDB"), 0o700); err != nil {
			t.Fatal(err)
		}
		e.root = empty
		wantError(t, e.cmd("sync"), 3, "no_teams_origin")
	})

	t.Run("usage", func(t *testing.T) {
		e := newEnv(t)
		e.sync() // the archive exists, so only the arguments can be at fault
		for _, args := range [][]string{
			{"bogus"},
			{"search"},
			{"messages", "--limit", "0"},
			{"messages", "--format", "xml"},
			{"messages", "--since", "yesterday-ish"},
			{"messages", "--max-age", "soon"},
			{"sync", "--fields", "id"},
			{"messages", "--fields", "nope"},
			{"thread", chat1},
			{"sql", "delete from messages"},
		} {
			wantError(t, e.cmd(args...), 2, "usage")
		}
	})

	t.Run("archive locked", func(t *testing.T) {
		e := newEnv(t)
		holder := e.start([]string{"TEAMSCRAWL_TEST_PAUSE_AFTER_SNAPSHOT=10s"}, append([]string{"sync"}, e.baseArgs()...)...)
		holder.waitFor("the pause", func() bool { return strings.Contains(holder.stderr.String(), pausedMarker) })
		errBody := wantError(t, e.cmd("sync"), 4, "locked")
		if !strings.Contains(errBody["message"].(string), ".lock") {
			t.Fatalf("locked message = %v", errBody["message"])
		}
		if runtime.GOOS == "windows" {
			if err := holder.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			<-holder.done
		} else {
			holder.signal(syscall.SIGINT)
		}
	})

	t.Run("cache cannot be copied consistently", func(t *testing.T) {
		e := newEnv(t)
		matches, _ := filepath.Glob(filepath.Join(e.root, "*", "IndexedDB", "*.leveldb", "MANIFEST-*"))
		if len(matches) != 1 {
			t.Fatalf("manifest files: %v", matches)
		}
		if err := os.Remove(matches[0]); err != nil {
			t.Fatal(err)
		}
		wantError(t, e.cmd("sync"), 1, "snapshot_inconsistent")
		// The attempt is recorded as failed and the archive is still readable.
		// (stderr carries the "run teamscrawl sync" hint: no run has succeeded.)
		if st := lastRunStatuses(t, e); len(st) != 1 || st[0] != "failed" {
			t.Fatalf("sync_runs statuses = %v, want [failed]", st)
		}
	})

	t.Run("a missing archive directory and a missing root", func(t *testing.T) {
		// The environment error wins, and nothing prints a stack trace (wantError checks that).
		e := newEnv(t)
		res := e.run("sync", "--db", filepath.Join(e.tmp, "nodir", "a", "b.db"), "--teams-root", filepath.Join(e.tmp, "missing"))
		wantError(t, res, 3, "teams_not_installed")
	})
}

func TestE2EDBModes(t *testing.T) {
	e := newEnv(t)
	e.sync()
	if runtime.GOOS == "windows" {
		for _, path := range []string{filepath.Dir(e.db), e.db} {
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
		}
		d := newEnv(t)
		mustExit(t, d.run("sync", "--teams-root", d.root), 0)
		for _, path := range []string{filepath.Dir(d.defaultArchivePath()), d.defaultArchivePath()} {
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	dir := filepath.Dir(e.db)
	for path, want := range map[string]os.FileMode{dir: 0o700, e.db: 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
	// Companion files (WAL, lock) must not be wider than the archive.
	for _, g := range []string{e.db + "-*", e.db + ".lock"} {
		m, _ := filepath.Glob(g)
		for _, p := range m {
			if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&0o077 != 0 {
				t.Errorf("%s mode = %o, want no group or other access", p, fi.Mode().Perm())
			}
		}
	}
	// The default location under HOME gets the same modes.
	d := newEnv(t)
	mustExit(t, d.run("sync", "--teams-root", d.root), 0)
	for path, want := range map[string]os.FileMode{
		filepath.Join(d.home, ".teamscrawl"):                  0o700,
		filepath.Join(d.home, ".teamscrawl", "teamscrawl.db"): 0o600,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}

func TestE2ENoSnapshotLeft(t *testing.T) {
	t.Run("after success", func(t *testing.T) {
		e := newEnv(t)
		e.sync()
		if m := snapshots(t, e.tmp); len(m) != 0 {
			t.Fatalf("snapshot left behind: %v", m)
		}
	})
	t.Run("after failure", func(t *testing.T) {
		e := newEnv(t)
		matches, _ := filepath.Glob(filepath.Join(e.root, "*", "IndexedDB", "*.leveldb", "MANIFEST-*"))
		for _, m := range matches {
			if err := os.Remove(m); err != nil {
				t.Fatal(err)
			}
		}
		// The failure is inside the snapshot step (the copy is retried and then given up), so a
		// test cannot observe the snapshot directory deterministically; the SIGINT case below
		// proves cleanup of a completed snapshot. Here we pin the error and the recorded attempt.
		wantError(t, e.cmd("sync"), 1, "snapshot_inconsistent")
		if m := snapshots(t, e.tmp); len(m) != 0 {
			t.Fatalf("snapshot left behind: %v", m)
		}
		if st := lastRunStatuses(t, e); len(st) != 1 || st[0] != "failed" {
			t.Fatalf("sync_runs statuses = %v, want [failed]", st)
		}
	})
	t.Run("after SIGINT mid-sync", func(t *testing.T) {
		skipIfWindowsSubprocessSignals(t)
		e := newEnv(t)
		// A 10 s pause is far longer than the test needs: it signals as soon as the marker shows
		// that the snapshot is complete and the process is waiting.
		s := e.start([]string{"TEAMSCRAWL_TEST_PAUSE_AFTER_SNAPSHOT=10s"}, append([]string{"sync"}, e.baseArgs()...)...)
		s.waitFor("the pause", func() bool { return strings.Contains(s.stderr.String(), pausedMarker) })
		snaps := snapshots(t, e.tmp)
		if len(snaps) != 1 {
			t.Fatalf("snapshots during the pause = %v, want exactly one", snaps)
		}
		fi, err := os.Stat(snaps[0])
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("snapshot dir mode = %o, want 700", fi.Mode().Perm())
		}
		start := time.Now()
		res := s.signal(syscall.SIGINT)
		if time.Since(start) > 8*time.Second {
			t.Errorf("SIGINT took %v; the pause was not interrupted", time.Since(start))
		}
		// An interrupted sync is a runtime failure (exit 1) with one coded "interrupted" error
		// after the marker.
		if res.code != 1 {
			t.Fatalf("exit = %d, want 1\nstderr: %s", res.code, res.stderr)
		}
		rest := strings.TrimSpace(strings.Replace(res.stderr, pausedMarker, "", 1))
		body := mustJSON(t, rest)
		ee, _ := body["error"].(map[string]any)
		if ee == nil || ee["code"] != "interrupted" || ee["message"] != "interrupted before finishing; nothing was half-written" || ee["fix"] != "Run the command again." {
			t.Fatalf("stderr error = %s", rest)
		}
		if res.stdout != "" {
			t.Fatalf("an interrupted sync printed a report: %s", res.stdout)
		}
		if m := snapshots(t, e.tmp); len(m) != 0 {
			t.Fatalf("snapshot left behind after SIGINT: %v", m)
		}
		if st := lastRunStatuses(t, e); len(st) != 1 || st[0] != "failed" {
			t.Fatalf("sync_runs statuses = %v, want [failed]", st)
		}
		// The interrupted run released the lock and wrote nothing: the next sync is a full one.
		if rep := e.sync(); rep["status"] != "ok" {
			t.Fatalf("sync after the interrupt = %v", rep["status"])
		}
	})
}

// lastRunStatuses lists the recorded sync_runs statuses, oldest first.
func lastRunStatuses(t *testing.T, e *env) []string {
	t.Helper()
	res := e.cmd("sql", "--max-age", "0", "select status from sync_runs where accounts_json is not null order by id")
	mustExit(t, res, 0) // stderr may carry the "run teamscrawl sync" hint: no run has succeeded
	var out []string
	for _, r := range mustJSON(t, res.stdout)["rows"].([]any) {
		out = append(out, r.([]any)[0].(string))
	}
	return out
}

func TestE2EActivity(t *testing.T) {
	e := newEnv(t)
	e.sync()

	all, _ := list(t, e.cmd("activity"))
	if len(all) != 22 {
		t.Fatalf("activity items = %d, want 22", len(all))
	}
	for _, k := range []string{"id", "type", "is_read", "at", "conversation_display_name", "sender_name", "text"} {
		if _, has := all[0][k]; !has {
			t.Errorf("activity item has no %q: %v", k, all[0])
		}
	}
	unread, _ := list(t, e.cmd("activity", "--unread"))
	if len(unread) != 14 {
		t.Fatalf("unread activity = %d, want 14", len(unread))
	}
	for _, it := range unread {
		if it["is_read"] != false {
			t.Fatalf("--unread returned a read item: %v", it)
		}
	}
	mentions, _ := list(t, e.cmd("activity", "--type", "mentionInChat"))
	if len(mentions) != 4 || mentions[3]["text"] != "Alex Fixture and Sam Tag see this" {
		t.Fatalf("mentionInChat = %v", mentions)
	}
	one, _ := list(t, e.cmd("activity", "--account", account1))
	if len(one) != 11 {
		t.Fatalf("account 1 activity = %d, want 11", len(one))
	}
	newest, whole := list(t, e.cmd("activity", "--limit", "1"))
	if len(newest) != 1 || whole["truncated"] != true {
		t.Fatalf("--limit 1 = %v truncated=%v", newest, whole["truncated"])
	}
}

func TestE2EUnread(t *testing.T) {
	e := newEnv(t)
	e.sync()

	def, whole := list(t, e.cmd("unread"))
	if len(def) != 12 || whole["channels_excluded"] != true {
		t.Fatalf("default unread = %d items, channels_excluded=%v; want 12 and true", len(def), whole["channels_excluded"])
	}
	for _, it := range def {
		if it["conversation_id"] == channel1 || it["conversation_id"] == "19:planningchannel1@thread.tacv2" {
			t.Fatalf("a channel message in the default unread list: %v", it)
		}
	}
	inc, whole := list(t, e.cmd("unread", "--include-channels"))
	if len(inc) != 16 {
		t.Fatalf("--include-channels = %d, want 16", len(inc))
	}
	if _, has := whole["channels_excluded"]; has {
		t.Fatalf("channels_excluded must be absent with --include-channels: %v", whole)
	}

	by, whole := list(t, e.cmd("unread", "--by-conversation"))
	if len(by) != 2 || whole["channels_excluded"] != true {
		t.Fatalf("--by-conversation = %v", by)
	}
	for _, it := range by {
		if num(t, it, "unread_count") != 6 || it["kind"] != "Chat" || !strings.HasPrefix(it["link"].(string), "https://teams.microsoft.com/l/message/") {
			t.Fatalf("by-conversation item = %v", it)
		}
		for _, k := range []string{"conversation_id", "conversation_display_name", "oldest_unread_at", "newest_unread_at"} {
			if _, has := it[k]; !has {
				t.Fatalf("by-conversation item has no %q: %v", k, it)
			}
		}
	}
	byAll, _ := list(t, e.cmd("unread", "--by-conversation", "--include-channels"))
	if len(byAll) != 4 {
		t.Fatalf("--by-conversation --include-channels = %d, want 4", len(byAll))
	}
	one, _ := list(t, e.cmd("unread", "--account", account2, "--fields", "tenant_id"))
	if len(one) != 6 {
		t.Fatalf("account 2 unread = %d, want 6", len(one))
	}

	// messages --unread follows the same rule.
	mu, whole := list(t, e.cmd("messages", "--unread"))
	if len(mu) != 12 || whole["channels_excluded"] != true {
		t.Fatalf("messages --unread = %d channels_excluded=%v", len(mu), whole["channels_excluded"])
	}
	mi, _ := list(t, e.cmd("messages", "--unread", "--include-channels"))
	if len(mi) != 16 {
		t.Fatalf("messages --unread --include-channels = %d, want 16", len(mi))
	}
}

func TestE2EThread(t *testing.T) {
	e := newEnv(t)
	e.sync()

	items, _ := list(t, e.cmd("thread", channel1, "1700000045000"))
	ids := strs(items, "id")
	if len(ids) != 2 || ids[0] != "1700000045000" || ids[1] != "1700000046000" {
		t.Fatalf("thread ids = %v", ids)
	}
	if items[0]["subject"] != "Fixture subject" || items[0]["importance"] != "high" || items[0]["pinned"] != true || items[1]["parent_message_id"] != "1700000045000" {
		t.Fatalf("thread items = %v", items)
	}
	limited, whole := list(t, e.cmd("thread", channel1, "1700000045000", "--limit", "1"))
	if len(limited) != 1 || whole["truncated"] != true {
		t.Fatalf("--limit 1 = %v truncated=%v", limited, whole["truncated"])
	}
	link, _ := list(t, e.cmd("thread", "https://teams.microsoft.com/l/message/"+channel1+"/1700000045000"))
	if len(link) != 2 {
		t.Fatalf("thread by link = %d items", len(link))
	}
	wantError(t, e.cmd("thread", channel1), 2, "usage")
	wantError(t, e.cmd("thread", "https://example.org/not-teams"), 2, "usage")
}

func TestE2EWhoami(t *testing.T) {
	e := newEnv(t)
	res := e.cmd("whoami", "--max-age", "0")
	mustExit(t, res, 0)
	if res.stderr != "" {
		t.Fatalf("JSON mode keeps stderr empty (the hint is in the result): %q", res.stderr)
	}
	who := mustJSON(t, res.stdout)
	if who["needs_sync"] != true || who["hint"] != "run teamscrawl sync" {
		t.Fatalf("whoami before a sync lacks the in-band hint: %v", who)
	}
	if accts, _ := who["accounts"].([]any); len(accts) != 0 {
		t.Fatalf("accounts before a sync = %v", who["accounts"])
	}
	if who["needs_sync"] != true || who["hint"] == "" {
		t.Fatalf("never synced: %v", who)
	}
	e.sync()
	who = ok(t, e.cmd("whoami"))
	accts, _ := who["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("accounts = %v", who["accounts"])
	}
	a := accts[0].(map[string]any)
	for _, k := range []string{"tenant_id", "user_id", "self_id", "display_name", "locale", "first_seen_at", "last_synced_at"} {
		if _, has := a[k]; !has {
			t.Errorf("account has no %q: %v", k, a)
		}
	}
	arch, _ := who["archive"].(map[string]any)
	if arch["archive_exists"] != true || arch["fts_present"] != true {
		t.Fatalf("archive = %v", arch)
	}
	if _, has := who["archive_age_seconds"]; !has {
		t.Fatalf("no archive_age_seconds: %v", who)
	}
}

func TestE2EMentionsMe(t *testing.T) {
	e := newEnv(t)
	e.sync()
	items, _ := list(t, e.cmd("messages", "--mentions-me"))
	if len(items) != 12 {
		t.Fatalf("messages --mentions-me = %d, want 12", len(items))
	}
	for _, it := range items {
		if it["mentions_me"] != true {
			t.Fatalf("item without mentions_me: %v", it)
		}
	}
	one, _ := list(t, e.cmd("messages", "--mentions-me", "--account", account2))
	if len(one) != 6 {
		t.Fatalf("account 2 mentions = %d, want 6", len(one))
	}
	s, _ := list(t, e.cmd("search", "Alex", "--mentions-me"))
	if len(s) != 2 {
		t.Fatalf("search --mentions-me = %d, want 2", len(s))
	}
}

func TestE2EMaxAge(t *testing.T) {
	t.Run("a never-synced archive syncs implicitly", func(t *testing.T) {
		e := newEnv(t)
		res := e.cmd("messages", "-c", "Fixture chat 1")
		mustExit(t, res, 0)
		if n := mustJSON(t, strings.TrimSpace(res.stderr)); n["notice"] != "syncing" || n["reason"] != "never_synced" || n["max_age_seconds"] != float64(900) || n["archive_age_seconds"] != nil {
			t.Fatalf("stderr = %q, want the sync notice alone, as JSON", res.stderr)
		}
		whole := mustJSON(t, res.stdout)
		if items, _ := whole["items"].([]any); len(items) == 0 {
			t.Fatal("the implicit sync produced no messages")
		}
		if age, ok := whole["archive_age_seconds"].(float64); !ok || age > 30 {
			t.Fatalf("archive_age_seconds = %v, want a small number", whole["archive_age_seconds"])
		}
		if _, has := whole["needs_sync"]; has {
			t.Fatalf("needs_sync after a successful implicit sync: %v", whole)
		}
	})

	t.Run("0 disables the implicit sync", func(t *testing.T) {
		e := newEnv(t)
		res := e.cmd("messages", "--max-age", "0")
		mustExit(t, res, 0)
		whole := mustJSON(t, res.stdout)
		if whole["needs_sync"] != true || whole["hint"] != "run teamscrawl sync" || whole["archive_age_seconds"] != nil || whole["count"] != float64(0) {
			t.Fatalf("never synced with --max-age 0 = %v", whole)
		}
		if _, err := os.Stat(e.db); err == nil {
			t.Fatal("--max-age 0 must not create the archive")
		}
		if strings.Contains(res.stderr, "hint:") {
			t.Fatalf("no plain-text hint line in JSON mode: %q", res.stderr)
		}
		// The same through the environment.
		res = e.runWith([]string{"TEAMSCRAWL_MAX_AGE=0"}, append([]string{"people"}, e.baseArgs()...)...)
		if mustExit(t, res, 0); mustJSON(t, res.stdout)["needs_sync"] != true {
			t.Fatalf("TEAMSCRAWL_MAX_AGE=0: %s", res.stdout)
		}
	})

	t.Run("a fresh archive is not synced again", func(t *testing.T) {
		e := newEnv(t)
		e.sync()
		// Break the Teams root: a read that tried to sync would warn.
		bad := []string{"--teams-root", filepath.Join(e.tmp, "missing"), "--db", e.db}
		res := e.run(append([]string{"people"}, bad...)...)
		_, whole := list(t, res)
		if _, has := whole["sync_error"]; has || res.stderr != "" {
			t.Fatalf("a fresh archive triggered a sync: %v / %q", whole, res.stderr)
		}
	})

	t.Run("a failing implicit sync warns and still returns results", func(t *testing.T) {
		e := newEnv(t)
		e.sync()
		// --max-age 1ns makes any archive stale without sleeping: the last successful sync is
		// always older than a nanosecond by the time the next process starts.
		bad := []string{"--teams-root", filepath.Join(e.tmp, "missing"), "--db", e.db}
		res := e.run(append([]string{"people", "--max-age", "1ns"}, bad...)...)
		mustExit(t, res, 0)
		whole := mustJSON(t, res.stdout)
		if n, _ := whole["items"].([]any); len(n) == 0 {
			t.Fatalf("no results from the stale archive: %s", res.stdout)
		}
		se, _ := whole["sync_error"].(map[string]any)
		if se == nil || se["code"] != "teams_not_installed" || se["message"] == "" {
			t.Fatalf("sync_error = %v", whole["sync_error"])
		}
		lines := strings.Split(strings.TrimSpace(res.stderr), "\n")
		if len(lines) != 2 {
			t.Fatalf("stderr = %q, want the notice then the warning", res.stderr)
		}
		if n := mustJSON(t, lines[0]); n["notice"] != "syncing" || n["reason"] != "stale" || n["archive_age_seconds"] == nil {
			t.Fatalf("notice = %s", lines[0])
		}
		warn := mustJSON(t, lines[1])
		w, _ := warn["warning"].(map[string]any)
		if w == nil || w["code"] != "teams_not_installed" || w["fix"] == "" {
			t.Fatalf("stderr warning = %s", res.stderr)
		}
		if _, has := warn["error"]; has {
			t.Fatalf("a warning must not be an error: %s", res.stderr)
		}
	})
}

func TestE2EFieldsAndMaxText(t *testing.T) {
	e := newEnv(t)
	e.sync()

	items, _ := list(t, e.cmd("messages", "-c", "Fixture chat 1", "--fields", "id,text"))
	for _, it := range items {
		if len(it) != 2 || it["id"] == nil || it["text"] == nil {
			t.Fatalf("--fields id,text kept %v", it)
		}
	}
	for _, args := range [][]string{
		{"search", "Hello", "--fields", "id"},
		{"conversations", "--fields", "id,display_name"},
		{"people", "--fields", "display_name"},
		{"activity", "--fields", "id,type"},
		{"unread", "--fields", "id"},
		{"thread", channel1, "1700000045000", "--fields", "id"},
	} {
		its, _ := list(t, e.cmd(args...))
		if len(its) == 0 {
			t.Fatalf("%v: no items", args)
		}
		want := len(strings.Split(args[len(args)-1], ","))
		for _, it := range its {
			if len(it) != want {
				t.Fatalf("%v kept %v", args, it)
			}
		}
	}

	cut, _ := list(t, e.cmd("messages", "-c", "Fixture chat 1", "--max-text", "5", "--fields", "id,text"))
	var truncated int
	for _, it := range cut {
		// text_truncated travels with text, so --fields id,text keeps it.
		if it["text_truncated"] == true {
			truncated++
		}
	}
	full, _ := list(t, e.cmd("messages", "-c", "Fixture chat 1", "--max-text", "5"))
	var flagged int
	for _, it := range full {
		text, _ := it["text"].(string)
		if n := len([]rune(text)); n > 6 { // 5 characters and the ellipsis
			t.Fatalf("text not truncated to 5: %q", text)
		}
		if it["text_truncated"] == true {
			flagged++
			if !strings.HasSuffix(text, "…") {
				t.Fatalf("truncated text without an ellipsis: %q", text)
			}
		}
	}
	if flagged == 0 || truncated == 0 {
		t.Fatalf("no item was truncated (flagged=%d)", flagged)
	}
	whole, _ := list(t, e.cmd("messages", "-c", "Fixture chat 1", "--max-text", "0"))
	for _, it := range whole {
		if _, has := it["text_truncated"]; has {
			t.Fatalf("--max-text 0 truncated: %v", it)
		}
	}

	// Bad keys and non-list commands are usage errors with a fix.
	wantError(t, e.cmd("messages", "--fields", "id,text_truncated"), 2, "usage")
	wantError(t, e.cmd("whoami", "--fields", "id"), 2, "usage")
	wantError(t, e.cmd("status", "--max-text", "5"), 2, "usage")
	wantError(t, e.cmd("messages", "--max-text", "-1"), 2, "usage")
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func TestE2EOutputContract(t *testing.T) {
	e := newEnv(t)
	e.sync()

	t.Run("non-TTY default is JSON", func(t *testing.T) {
		res := e.cmd("conversations")
		mustExit(t, res, 0)
		if !json.Valid([]byte(res.stdout)) || !strings.HasPrefix(res.stdout, "{") {
			t.Fatalf("default output is not JSON: %s", res.stdout)
		}
		if strings.Count(res.stdout, "\n") != 1 {
			t.Fatalf("JSON output should be one line: %q", res.stdout)
		}
	})

	t.Run("--format text prints no JSON", func(t *testing.T) {
		for _, args := range [][]string{{"conversations"}, {"messages", "-c", "Fixture chat 1"}, {"status"}, {"whoami"}, {"doctor"}, {"unread", "--by-conversation"}} {
			res := e.cmd(append(args, "--format", "text")...)
			mustExit(t, res, 0)
			if json.Valid([]byte(strings.TrimSpace(res.stdout))) || strings.HasPrefix(strings.TrimSpace(res.stdout), "{") {
				t.Fatalf("%v: text mode printed JSON: %.200s", args, res.stdout)
			}
			if strings.TrimSpace(res.stdout) == "" {
				t.Fatalf("%v: text mode printed nothing", args)
			}
		}
		res := e.cmd("conversations", "--format", "text")
		if !strings.Contains(res.stdout, "Fixture chat 1") {
			t.Fatalf("text output misses the data: %s", res.stdout)
		}
	})

	t.Run("--json overrides text", func(t *testing.T) {
		res := e.cmd("conversations", "--json")
		if mustExit(t, res, 0); !json.Valid([]byte(res.stdout)) {
			t.Fatalf("--json did not print JSON: %s", res.stdout)
		}
	})

	t.Run("no ANSI escapes without a terminal, with NO_COLOR or --no-color", func(t *testing.T) {
		base := append([]string{"conversations", "--format", "text"}, e.baseArgs()...)
		for name, tc := range map[string]struct {
			env  []string
			args []string
		}{
			"piped":                  {nil, nil},
			"NO_COLOR":               {[]string{"NO_COLOR=1"}, nil},
			"--no-color":             {nil, []string{"--no-color"}},
			"NO_COLOR beats force":   {[]string{"NO_COLOR=1", "CLICOLOR_FORCE=1"}, nil},
			"--no-color beats force": {[]string{"CLICOLOR_FORCE=1"}, []string{"--no-color"}},
		} {
			res := e.runWith(tc.env, append(append([]string{}, base...), tc.args...)...)
			mustExit(t, res, 0)
			if ansi.MatchString(res.stdout) || ansi.MatchString(res.stderr) {
				t.Errorf("%s: ANSI escapes in output: %q", name, res.stdout)
			}
		}
		// Sanity: forcing color really does color, so the checks above prove something.
		res := e.runWith([]string{"CLICOLOR_FORCE=1"}, base...)
		if !ansi.MatchString(res.stdout) {
			t.Errorf("CLICOLOR_FORCE=1 produced no color; the no-color assertions are vacuous")
		}
	})

	t.Run("errors are coded on stderr: JSON by default, lines in text mode", func(t *testing.T) {
		for _, format := range []string{"json", "text"} {
			e2 := newEnv(t)
			e2.root = filepath.Join(e2.tmp, "missing")
			res := e2.cmd("sync", "--format", format)
			if format == "json" {
				body := wantError(t, res, 3, "teams_not_installed")
				if !strings.Contains(body["fix"].(string), "Teams") {
					t.Fatalf("fix = %v", body["fix"])
				}
				continue
			}
			// Text mode prints the same error as "error:" and "fix:" lines, still on stderr.
			mustExit(t, res, 3)
			if res.stdout != "" || !strings.Contains(res.stderr, "error: Teams desktop data not found") || !strings.Contains(res.stderr, "fix: Install") {
				t.Fatalf("text-mode error: stdout %q stderr %q", res.stdout, res.stderr)
			}
			if json.Valid([]byte(strings.TrimSpace(res.stderr))) {
				t.Fatalf("text-mode stderr is JSON: %s", res.stderr)
			}
		}
	})
}

// TestE2EWatch runs the real watch loop: the baseline sync prints nothing, then a change in the
// (copied) cache produces a message line.
func TestE2EWatch(t *testing.T) {
	skipIfWindowsSubprocessSignals(t)
	e := newEnv(t)
	s := e.start(nil, append([]string{"watch", "--every", "1s"}, e.baseArgs()...)...)
	count := func(q string) int { return archiveCount(t, e.db, q) }
	// The baseline is done once its sync run is recorded as successful (it is written last).
	s.waitFor("the baseline sync", func() bool {
		return count("select count(*) from sync_runs where status='ok' and accounts_json is not null") == 1 && count("select count(*) from messages") == 110
	})
	if out := s.stdout.String(); out != "" {
		t.Fatalf("the baseline must print nothing, got %q", out)
	}

	// Make the next sync see one message as new: forget it in the archive, then touch the cache so
	// its fingerprint changes.
	archiveExec(t, e.db, `delete from message_fts where rowid in (select rowid from messages where id='1700000001000' and tenant_id=?)`, tenant1)
	archiveExec(t, e.db, `delete from messages where id='1700000001000' and tenant_id=?`, tenant1)
	logs, _ := filepath.Glob(filepath.Join(e.root, "*", "IndexedDB", "*.leveldb", "*.log"))
	if len(logs) == 0 {
		t.Fatal("no leveldb log to touch")
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(logs[0], later, later); err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	s.waitFor("a message line", func() bool {
		for _, l := range strings.Split(s.stdout.String(), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(l), &m) == nil && m["kind"] == "message" {
				line = m
				return true
			}
		}
		return false
	})
	// Exactly one message line in all: the baseline emitted none, and only the forgotten message
	// came back.
	var messageLines int
	for _, l := range strings.Split(s.stdout.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["kind"] == "message" {
			messageLines++
		}
	}
	if messageLines != 1 {
		t.Fatalf("message lines = %d, want 1\n%s", messageLines, s.stdout.String())
	}
	if line["change"] != "new" {
		t.Fatalf("change = %v, want new: %v", line["change"], line)
	}
	item, _ := line["item"].(map[string]any)
	if item["id"] != "1700000001000" || item["text"] != "Hello from Alex Fixture" {
		t.Fatalf("item = %v", item)
	}
	res := s.signal(syscall.SIGTERM)
	if res.code != 0 {
		t.Fatalf("exit %d after SIGTERM\nstderr: %s", res.code, res.stderr)
	}
	for _, l := range strings.Split(strings.TrimSpace(res.stdout), "\n") {
		if !json.Valid([]byte(l)) {
			t.Fatalf("watch printed a non-JSON line: %q", l)
		}
	}
	if m := snapshots(t, e.tmp); len(m) != 0 {
		t.Fatalf("snapshot left behind: %v", m)
	}
}

func archiveCount(t *testing.T, path, q string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 0
	}
	defer func() { _ = db.Close() }()
	var n int
	if db.QueryRow(q).Scan(&n) != nil {
		return 0 // the archive may not exist yet
	}
	return n
}

func archiveExec(t *testing.T, path, q string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// wantDoctorFailed checks the stderr half of a failed doctor run: a coded doctor_failed error.
// (doctor prints its checks on stdout, so wantError, which wants an empty stdout, does not apply.)
func wantDoctorFailed(t *testing.T, res result) {
	t.Helper()
	body := mustJSON(t, res.stderr)
	e, _ := body["error"].(map[string]any)
	if e == nil || e["code"] != "doctor_failed" || e["fix"] == "" {
		t.Fatalf("stderr is not a doctor_failed error: %s", res.stderr)
	}
}

func TestE2EPolish(t *testing.T) {
	e := newEnv(t)
	e.sync()
	// search with filters alone matches messages with the same filters.
	filtered, _ := list(t, e.cmd("search", "--mentions-me"))
	if len(filtered) != 12 {
		t.Fatalf("search --mentions-me = %d, want 12", len(filtered))
	}
	if res := e.cmd("search"); res.code != 2 || !strings.Contains(res.stderr, "teamscrawl messages") {
		t.Fatalf("bare search: exit %d %s", res.code, res.stderr)
	}
	// --team picks one account's team by name.
	team, _ := list(t, e.cmd("conversations", "--team", "fixture team 1"))
	if len(team) < 3 {
		t.Fatalf("--team conversations = %d", len(team))
	}
	for _, it := range team {
		if it["user_id"] != user1 {
			t.Errorf("--team leaked another account: %v", it)
		}
	}
	// total appears on a truncated list, equals the full count, and is absent otherwise.
	cut, whole := list(t, e.cmd("messages", "--limit", "2"))
	all, full := list(t, e.cmd("messages", "--limit", "500"))
	if len(cut) != 2 || whole["truncated"] != true || int(whole["total"].(float64)) != len(all) {
		t.Fatalf("total = %v, want %d", whole["total"], len(all))
	}
	if _, has := full["total"]; has {
		t.Fatalf("total on a complete list: %v", full["total"])
	}
	// Channel roots count their replies.
	var roots int
	for _, it := range all {
		if n, has := it["reply_count"].(float64); has && n == 1 {
			roots++
		}
	}
	if roots != 2 {
		t.Fatalf("channel roots with one reply = %d, want 2 (one per account)", roots)
	}
	// Cards and system events read as text.
	var card, call int
	for _, it := range all {
		switch it["text"] {
		case "Adaptive fixture card":
			card++
		case "Call ended":
			call++
		}
	}
	if card != 2 || call != 2 {
		t.Fatalf("card text %d, call text %d, want 2 each", card, call)
	}
}

// An archive stamped by a newer build is never written: sync and watch fail before any write, and
// reads still answer (an implicit sync degrades to a warning and a sync_error field).
func TestE2EArchiveNewer(t *testing.T) {
	e := newEnv(t)
	e.sync()
	archiveExec(t, e.db, `update meta set value='99' where key='derivation_version'`)
	rows := archiveCount(t, e.db, `select count(*) from messages`)
	runs := archiveCount(t, e.db, `select count(*) from sync_runs`)

	for _, args := range [][]string{{"sync"}, {"watch", "--every", "1h"}} {
		res := e.cmd(args...)
		if res.code != 3 || res.stdout != "" {
			t.Fatalf("%v: exit %d, stdout %q, stderr %s", args, res.code, res.stdout, res.stderr)
		}
		body, _ := mustJSON(t, res.stderr)["error"].(map[string]any)
		if body["code"] != "archive_newer" || !strings.Contains(body["message"].(string), "99") || !strings.Contains(body["fix"].(string), "brew upgrade ourostack/tap/teamscrawl") {
			t.Fatalf("%v: error = %v", args, body)
		}
	}
	res := e.run(append([]string{"people", "--max-age", "1ns"}, e.baseArgs()...)...)
	mustExit(t, res, 0)
	whole := mustJSON(t, res.stdout)
	se, _ := whole["sync_error"].(map[string]any)
	if n, _ := whole["items"].([]any); len(n) == 0 || se == nil || se["code"] != "archive_newer" {
		t.Fatalf("stale read on a newer archive: %s", res.stdout)
	}
	if got := archiveCount(t, e.db, `select count(*) from messages`); got != rows {
		t.Errorf("messages %d, was %d", got, rows)
	}
	if got := archiveCount(t, e.db, `select count(*) from sync_runs`); got != runs {
		t.Errorf("sync_runs %d, was %d: a refused sync must record nothing", got, runs)
	}
	if got := archiveCount(t, e.db, `select count(*) from meta where value='99'`); got != 1 {
		t.Error("the archive version changed")
	}
}

// twoProfiles gives the machine a second Teams profile (a copy of the first, same accounts) and
// returns the second profile's MANIFEST, which tests make unreadable to break that source.
func twoProfiles(t *testing.T, e *env) (manifest string) {
	t.Helper()
	src := filepath.Join(e.root, "WV2Profile_fixture")
	dst := filepath.Join(e.root, "WV2Profile_second")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		b, err := os.ReadFile(p) //nolint:gosec // G304: copying the test's own fixture copy
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600) //nolint:gosec // G703: under the test's temp dir
	})
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := filepath.Glob(filepath.Join(dst, "IndexedDB", "*.leveldb", "MANIFEST-*"))
	if len(ms) != 1 {
		t.Fatalf("manifests: %v", ms)
	}
	return ms[0]
}

// A sync in which one of two sources fails prints the report, then a coded partial_sync error, and
// the archive does not count as fresh: the next read says so and tries again.
func TestE2EPartialSync(t *testing.T) {
	skipIfRoot(t)
	skipIfWindowsPermissionSimulation(t)
	e := newEnv(t)
	manifest := twoProfiles(t, e)
	if err := os.Chmod(manifest, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(manifest, 0o600) })

	res := e.cmd("sync")
	mustExit(t, res, 1)
	rep := mustJSON(t, res.stdout)
	srcs, _ := rep["sources"].([]any)
	if rep["status"] != "partial" || len(srcs) != 2 {
		t.Fatalf("report: %s", res.stdout)
	}
	good, bad := srcs[0].(map[string]any), srcs[1].(map[string]any)
	if good["status"] != "ok" || bad["status"] != "failed" || !strings.Contains(bad["source"].(string), "WV2Profile_second") {
		t.Fatalf("sources: %v", srcs)
	}
	if be, _ := bad["error"].(map[string]any); be["code"] != "no_full_disk_access" || be["message"] == "" {
		t.Fatalf("failed source error: %v", bad)
	}
	if n := num(t, rep["messages"].(map[string]any), "inserted"); n != goldenCount(t, "mapped-messages.json") {
		t.Errorf("the committed source's messages inserted = %d", n)
	}
	body := mustJSON(t, res.stderr)["error"].(map[string]any)
	if body["code"] != "partial_sync" || !strings.Contains(body["message"].(string), "WV2Profile_second") || !strings.Contains(body["message"].(string), "no_full_disk_access") || !strings.Contains(body["fix"].(string), "teamscrawl doctor") {
		t.Fatalf("stderr error: %s", res.stderr)
	}
	if got := archiveCount(t, e.db, `select count(*) from messages`); got == 0 {
		t.Fatal("the committed source's rows must be in the archive")
	}
	if st := lastRunStatuses(t, e); len(st) == 0 || st[len(st)-1] != "partial" {
		t.Fatalf("run statuses = %v", st)
	}

	// The partial run did not refresh: a read tries the implicit sync again and reports why it is stale.
	runs := archiveCount(t, e.db, `select count(*) from sync_runs where accounts_json is not null`)
	res = e.cmd("people")
	mustExit(t, res, 0)
	whole := mustJSON(t, res.stdout)
	se, _ := whole["sync_error"].(map[string]any)
	if se["code"] != "partial_sync" || whole["archive_age_seconds"] != nil {
		t.Fatalf("read after a partial sync: %s", res.stdout)
	}
	if !strings.Contains(res.stderr, `"warning"`) {
		t.Fatalf("stderr = %q", res.stderr)
	}
	if got := archiveCount(t, e.db, `select count(*) from sync_runs where accounts_json is not null`); got != runs+1 {
		t.Fatalf("the read must attempt an implicit sync: runs %d -> %d", runs, got)
	}

	// Repair the source: the next sync is complete, and reads stop retrying.
	if err := os.Chmod(manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	rep = e.sync()
	if rep["status"] != "ok" {
		t.Fatalf("repaired sync: %v", rep)
	}
	runs = archiveCount(t, e.db, `select count(*) from sync_runs where accounts_json is not null`)
	whole = ok(t, e.cmd("people"))
	if whole["archive_age_seconds"] == nil || whole["sync_error"] != nil {
		t.Fatalf("after a full sync: %v", whole)
	}
	if got := archiveCount(t, e.db, `select count(*) from sync_runs where accounts_json is not null`); got != runs {
		t.Fatalf("a fresh archive must not sync again: %d -> %d", runs, got)
	}
}

// whoami's nested archive block carries the same age as the top level.
func TestE2EWhoamiNestedAge(t *testing.T) {
	e := newEnv(t)
	e.sync()
	who := ok(t, e.cmd("whoami"))
	nested, _ := who["archive"].(map[string]any)
	if who["archive_age_seconds"] == nil || nested["archive_age_seconds"] != who["archive_age_seconds"] {
		t.Fatalf("top %v, nested %v", who["archive_age_seconds"], nested["archive_age_seconds"])
	}
}

// A second SIGINT force-quits at once (exit 130) even when the graceful stop is stuck.
func TestE2EDoubleSIGINT(t *testing.T) {
	skipIfWindowsSubprocessSignals(t)
	e := newEnv(t)
	// The stubborn pause ignores cancellation, so the graceful stop cannot finish within the test.
	s := e.start([]string{"TEAMSCRAWL_TEST_PAUSE_AFTER_SNAPSHOT=60s", "TEAMSCRAWL_TEST_PAUSE_IGNORES_CANCEL=1"}, append([]string{"sync"}, e.baseArgs()...)...)
	s.waitFor("the pause", func() bool { return strings.Contains(s.stderr.String(), pausedMarker) })
	if err := s.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
		t.Fatalf("the first SIGINT must not end a stuck stop\nstderr: %s", s.stderr.String())
	case <-time.After(300 * time.Millisecond):
	}
	start := time.Now()
	res := s.signal(syscall.SIGINT)
	if res.code != 130 {
		t.Fatalf("exit = %d, want 130\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the second SIGINT took %v", time.Since(start))
	}
}

// An archive written before run-level sync_runs rows existed (no accounts_json column) is read
// without errors, and the next sync upgrades it.
func TestE2EArchiveFromAnOlderVersion(t *testing.T) {
	commands := [][]string{{"messages"}, {"status"}, {"whoami"}, {"sql", "select count(*) from messages"}, {"doctor"}}
	for _, mode := range []string{"default", "max-age-0"} {
		for _, c := range commands {
			t.Run(mode+"/"+c[0], func(t *testing.T) {
				e := newEnv(t)
				e.sync()
				makeAlpha1(t, e.db)
				args := append([]string{}, c...)
				if mode == "max-age-0" {
					args = append(args, "--max-age", "0")
				}
				res := e.cmd(args...)
				mustExit(t, res, 0)
				if strings.Contains(res.stderr, "db_error") || strings.Contains(res.stdout, "db_error") {
					t.Fatalf("db_error on an older archive: %s %s", res.stdout, res.stderr)
				}
				whole := mustJSON(t, res.stdout)
				if c[0] == "doctor" {
					for _, ch := range whole["checks"].([]any) {
						m := ch.(map[string]any)
						if m["name"] == "archive_upgrade" && m["warn"] == true {
							return
						}
					}
					t.Fatalf("doctor lacks the archive_upgrade warning: %s", res.stdout)
				}
				migrated := archiveCount(t, e.db, `select count(*) from pragma_table_info('sync_runs') where name='accounts_json'`) == 1
				if mode == "max-age-0" {
					if migrated || whole["needs_sync"] != true || whole["hint"] != "run teamscrawl sync" {
						t.Fatalf("--max-age 0 must not sync and must say needs_sync: migrated %v, %s", migrated, res.stdout)
					}
					return
				}
				if !migrated || whole["needs_sync"] != nil || whole["archive_age_seconds"] == nil {
					t.Fatalf("the implicit sync must upgrade the archive: migrated %v, %s", migrated, res.stdout)
				}
			})
		}
	}
}

// makeAlpha1 turns a current archive into the exact alpha.1 shape: no meta table and no
// sync_runs.accounts_json column.
func makeAlpha1(t *testing.T, path string) {
	t.Helper()
	archiveExec(t, path, `drop table meta`)
	archiveExec(t, path, `alter table sync_runs drop column accounts_json`)
}

func TestE2EAlpha1ArchiveUpgradeFlow(t *testing.T) {
	e := newEnv(t)
	e.sync()
	makeAlpha1(t, e.db)
	res := e.cmd("doctor")
	mustExit(t, res, 0)
	var warns []string
	for _, ch := range mustJSON(t, res.stdout)["checks"].([]any) {
		m := ch.(map[string]any)
		if m["ok"] != true {
			t.Fatalf("a failing check on an alpha.1 archive: %s", res.stdout)
		}
		if m["warn"] == true {
			warns = append(warns, m["name"].(string))
		}
	}
	if len(warns) != 1 || warns[0] != "archive_upgrade" {
		t.Fatalf("warnings %v: %s", warns, res.stdout)
	}
	e.sync()
	res = e.cmd("doctor")
	mustExit(t, res, 0)
	if strings.Contains(res.stdout, `"warn":true`) || strings.Contains(res.stdout, "archive_upgrade") {
		t.Fatalf("doctor must be clean after the sync: %s", res.stdout)
	}
}

// skill prints the embedded agent guide as raw Markdown in every output mode, with no archive or
// Teams cache needed, and it is the same text as the file in the repository.
func TestSkillPrintsTheGuide(t *testing.T) {
	want, err := os.ReadFile("../.agents/skills/teamscrawl/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	for _, args := range [][]string{{"skill"}, {"skill", "--json"}, {"skill", "--format", "text"}} {
		res := e.run(args...)
		mustExit(t, res, 0)
		if res.stdout != string(want) || res.stderr != "" {
			t.Fatalf("teamscrawl %v: stdout differs from SKILL.md (%d vs %d bytes), stderr %q", args, len(res.stdout), len(want), res.stderr)
		}
	}
}

// The friction round after the first fresh-agent run: sync notice, mention kinds, activity actors,
// the teams list and unread --since, each through the built binary.

func TestE2EImplicitSyncNotice(t *testing.T) {
	e := newEnv(t)
	res := e.cmd("unread", "--limit", "1")
	mustExit(t, res, 0)
	if n := mustJSON(t, strings.TrimSpace(res.stderr)); n["notice"] != "syncing" || n["reason"] != "never_synced" || strings.Contains(res.stdout, "notice") {
		t.Fatalf("stderr %q stdout %q", res.stderr, res.stdout)
	}
	whole := mustJSON(t, res.stdout)
	synced, _ := whole["synced"].(map[string]any)
	if synced["status"] != "ok" || synced["seconds"] == nil || whole["archive_age_seconds"] == nil {
		t.Fatalf("synced = %v, archive_age_seconds = %v", whole["synced"], whole["archive_age_seconds"])
	}
	// A second read finds the archive fresh: silent, and no synced key.
	res = e.cmd("unread", "--limit", "1")
	if _, has := ok(t, res)["synced"]; has {
		t.Fatalf("a fresh read reports a sync: %s", res.stdout)
	}
	// A stale archive: the JSON notice carries the age and the limit.
	archiveExec(t, e.db, `update sync_runs set finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now','-134 minutes','-30 seconds'), started_at=strftime('%Y-%m-%dT%H:%M:%fZ','now','-134 minutes','-31 seconds')`)
	res = e.cmd("unread", "--limit", "1")
	mustExit(t, res, 0)
	if n := mustJSON(t, strings.TrimSpace(res.stderr)); n["reason"] != "stale" || n["max_age_seconds"] != float64(900) || n["archive_age_seconds"].(float64) < 8040 || n["archive_age_seconds"].(float64) > 8100 {
		t.Fatalf("stale notice = %s", res.stderr)
	}
	// Text mode: the plain line on stderr, a clean table on stdout.
	archiveExec(t, e.db, `update sync_runs set finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now','-134 minutes','-30 seconds'), started_at=strftime('%Y-%m-%dT%H:%M:%fZ','now','-134 minutes','-31 seconds')`)
	res = e.cmd("--format", "text", "unread", "--limit", "1")
	mustExit(t, res, 0)
	if res.stderr != "teamscrawl: syncing — archive is 2h14m old (max-age 15m)\n" || strings.Contains(res.stdout, "teamscrawl: syncing") {
		t.Fatalf("text: stderr %q stdout %q", res.stderr, res.stdout)
	}
}

func TestE2EMentionKindAndDirectMentions(t *testing.T) {
	e := newEnv(t)
	e.sync()
	items, _ := list(t, e.cmd("messages", "--mentions-me", "--limit", "100"))
	kinds := map[string]int{}
	for _, it := range items {
		k, _ := it["mention_kind"].(string)
		kinds[k]++
	}
	want := map[string]int{"person": 4, "channel": 2, "team": 2, "tag": 2, "everyone": 2}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("mention kinds = %v, want %v", kinds, want)
	}
	for _, cmd := range [][]string{{"messages"}, {"search", "see this"}, {"search"}} {
		direct, _ := list(t, e.cmd(append(cmd, "--direct-mentions", "--limit", "100")...))
		if len(direct) == 0 {
			t.Fatalf("%v --direct-mentions found nothing", cmd)
		}
		for _, it := range direct {
			if it["mention_kind"] != "person" {
				t.Fatalf("%v --direct-mentions returned %v", cmd, it["mention_kind"])
			}
		}
	}
	acts, _ := list(t, e.cmd("activity", "--direct-mentions"))
	if len(acts) != 2 || acts[0]["subtype"] != "person" {
		t.Fatalf("activity --direct-mentions = %v", acts)
	}
}

func TestE2EActivityActors(t *testing.T) {
	e := newEnv(t)
	e.sync()
	acts, _ := list(t, e.cmd("--account", account1, "activity", "--limit", "100"))
	seen := map[string]bool{}
	for _, it := range acts {
		typ, _ := it["type"].(string)
		name, hasName := it["actor_name"]
		switch typ {
		case "reactionInChat", "mentionInChat", "reply", "replyToReply", "follow", "mention":
			if name != "Pat Example" || it["actor_id"] == nil {
				t.Errorf("%s: actor %v / %v", typ, it["actor_id"], name)
			}
			seen[typ] = true
		default:
			if hasName || it["actor_id"] != nil {
				t.Errorf("%s must have no actor: %v", typ, it)
			}
		}
	}
	if len(seen) != 6 {
		t.Fatalf("types with an actor = %v", seen)
	}
}

func TestE2ETeams(t *testing.T) {
	e := newEnv(t)
	e.sync()
	teams, whole := list(t, e.cmd("teams"))
	if len(teams) != 2 || whole["truncated"] != false {
		t.Fatalf("teams = %v", whole)
	}
	for _, tm := range teams {
		if tm["channel_count"] != float64(2) || tm["last_activity_at"] == nil || tm["unread_count"] == nil || tm["team_id"] == nil {
			t.Fatalf("team = %v", tm)
		}
	}
	one, _ := list(t, e.cmd("teams", "--account", account2, "--fields", "display_name,channel_count"))
	if len(one) != 1 || one[0]["display_name"] != "Fixture team 2" || len(one[0]) != 2 {
		t.Fatalf("--account/--fields: %v", one)
	}
	cut, whole := list(t, e.cmd("teams", "--limit", "1"))
	if len(cut) != 1 || whole["truncated"] != true || whole["total"] != float64(2) {
		t.Fatalf("--limit: %v", whole)
	}
	// The id and the name from the list both work as --team.
	for _, v := range []any{teams[0]["team_id"], teams[0]["display_name"]} {
		if convs, _ := list(t, e.cmd("conversations", "--team", v.(string))); len(convs) < 3 {
			t.Fatalf("--team %v: %d conversations", v, len(convs))
		}
	}
	res := e.cmd("messages", "--team", "No such team")
	mustExit(t, res, 2)
	if !strings.Contains(res.stderr, "teamscrawl teams") {
		t.Fatalf("the --team error does not point at `teams`: %s", res.stderr)
	}
}

func TestE2EUnreadSince(t *testing.T) {
	e := newEnv(t)
	e.sync()
	count := func(args ...string) float64 {
		t.Helper()
		_, whole := list(t, e.cmd(append([]string{"unread", "--limit", "1"}, args...)...))
		if total, ok := whole["total"].(float64); ok {
			return total
		}
		return whole["count"].(float64)
	}
	all := count()
	if all == 0 || count("--since", "2023-01-01") != all || count("--since", "7d") != 0 {
		t.Fatalf("unread %v, since 2023 %v, since 7d %v", all, count("--since", "2023-01-01"), count("--since", "7d"))
	}
	if mid := count("--since", "2023-11-14T22:14:00Z"); mid == 0 || mid >= all {
		t.Fatalf("a cutoff inside the fixture must keep some unread messages: %v of %v", mid, all)
	}
	by, whole := list(t, e.cmd("unread", "--by-conversation", "--include-channels", "--team", "Fixture team 1", "--since", "2023-01-01"))
	if len(by) == 0 || whole["channels_excluded"] != nil {
		t.Fatalf("by-conversation with team and channels: %v", whole)
	}
	if none, _ := list(t, e.cmd("unread", "--by-conversation", "--since", "7d")); len(none) != 0 {
		t.Fatalf("by-conversation since 7d = %v", none)
	}
}
