package cli

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

var (
	reAge = regexp.MustCompile(`\d+(\.\d+)?(ms|µs|s|m|h)+ ago`)
)

// scrub makes text output stable: paths and clocks differ per run.
func (e *env) scrub(s string) string {
	s = strings.ReplaceAll(s, e.root, "<teams-root>")
	s = strings.ReplaceAll(s, e.db, "<db>")
	s = strings.ReplaceAll(s, filepath.Dir(e.db), "<db-dir>")
	s = reAge.ReplaceAllString(s, "<age> ago")
	for re, to := range map[string]string{
		`(last sync *(?:\x1b\[0m)? +)\d{4}-\d\d-\d\d \d\d:\d\d`:   "${1}<stamp>",
		`(archive age *(?:\x1b\[0m)?  +)[0-9.a-zµ]+`:              "${1}<age>",
		`archive age: [0-9.a-zµ]+`:                                "archive age: <age>",
		`ok at \d{4}-\d\d-\d\d \d\d:\d\d`:                         "ok at <stamp>",
		`(?m)^(\S+ Fixture +\S+ +\S+ +)\d{4}-\d\d-\d\d \d\d:\d\d`: "${1}<stamp>",
	} {
		s = regexp.MustCompile(re).ReplaceAllString(s, to)
	}
	return s
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed testdata name
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if string(want) != got {
		t.Errorf("%s mismatch\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

func textGoldenName(name string) string {
	if name == "doctor" && goruntime.GOOS == "windows" {
		return name + ".windows"
	}
	return name
}

func textEnv(t *testing.T) *env {
	t.Helper()
	old := displayZone
	displayZone = time.UTC
	t.Cleanup(func() { displayZone = old })
	t.Setenv("COLUMNS", "100")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "")
	return newEnv(t)
}

func TestTextGoldens(t *testing.T) {
	e := textEnv(t)
	e.sync()
	cases := []struct {
		name string
		args []string
	}{
		{"doctor", []string{"doctor"}},
		{"search", []string{"search", "Hello"}},
		{"conversations", []string{"conversations"}},
		{"status", []string{"status"}},
		{"whoami", []string{"whoami"}},
		{"people_fields", []string{"--fields", "display_name,last_seen_at", "people"}},
		{"sql", []string{"sql", "select id from messages order by id limit 2"}},
		{"activity", []string{"activity"}},
	}
	for _, c := range cases {
		for _, color := range []bool{false, true} {
			args := append([]string{"--format", "text", "--max-age", "0"}, c.args...)
			suffix := "plain"
			t.Setenv("CLICOLOR_FORCE", "")
			if color {
				suffix = "color"
				t.Setenv("CLICOLOR_FORCE", "1")
			}
			code, out, errOut := e.run(args...)
			if code != 0 {
				t.Fatalf("%s: exit %d: %s", c.name, code, errOut)
			}
			if !color && strings.Contains(out, "\x1b") {
				t.Errorf("%s: escape in plain output", c.name)
			}
			if color && !strings.Contains(out, "\x1b[") {
				t.Errorf("%s: no color with CLICOLOR_FORCE=1", c.name)
			}
			checkGolden(t, textGoldenName(c.name)+"."+suffix, e.scrub(out))
		}
	}
}

func TestTextDoctorSyncAndBanner(t *testing.T) {
	e := textEnv(t)
	code, out, _ := e.run("--format", "text", "sync")
	if code != 0 || !strings.Contains(out, "local-first Teams mirror for SQLite  |  sync") {
		t.Fatalf("sync banner: %d %q", code, out)
	}
	_, out, _ = e.run("--format", "text", "--max-age", "0", "search", "Hello")
	if strings.Contains(out, "local-first") {
		t.Errorf("list commands carry no banner: %q", out)
	}
	_, out, _ = e.run("--format", "text", "--help")
	if !strings.Contains(out, "|  help") || !strings.Contains(out, "Usage: teamscrawl") {
		t.Errorf("help: %q", out)
	}
	_, out, _ = e.run("--format", "json", "--help")
	if strings.Contains(out, "local-first") {
		t.Errorf("json help must not carry the banner")
	}
}

func TestHelpHonorsNoColor(t *testing.T) {
	e := textEnv(t)
	t.Setenv("CLICOLOR_FORCE", "1")
	for _, args := range [][]string{
		{"--format", "text", "--no-color", "--help"},
		{"--format", "text", "--help", "--no-color"},
		{"--format", "text", "doctor", "--help", "--no-color"},
	} {
		_, out, _ := e.run(args...)
		if !strings.Contains(out, "|  help") || strings.Contains(out, "\x1b") {
			t.Errorf("%v: want plain banner, got %q", args, out)
		}
	}
	_, out, _ := e.run("--format", "text", "--help")
	if !strings.Contains(out, "\x1b[") {
		t.Error("CLICOLOR_FORCE must color help when --no-color is absent")
	}
	t.Setenv("NO_COLOR", "1")
	if _, out, _ = e.run("--format", "text", "--help"); strings.Contains(out, "\x1b") {
		t.Error("NO_COLOR must win over CLICOLOR_FORCE in help")
	}
}

func TestColorSwitches(t *testing.T) {
	e := textEnv(t)
	e.sync()
	run := func(args ...string) string {
		_, out, _ := e.run(append([]string{"--format", "text", "--max-age", "0"}, args...)...)
		return out
	}
	t.Setenv("CLICOLOR_FORCE", "1")
	if !strings.Contains(run("conversations"), "\x1b[") {
		t.Error("CLICOLOR_FORCE must color")
	}
	if strings.Contains(run("--no-color", "conversations"), "\x1b") {
		t.Error("--no-color must win over CLICOLOR_FORCE")
	}
	t.Setenv("NO_COLOR", "1")
	if strings.Contains(run("conversations"), "\x1b") {
		t.Error("NO_COLOR must win over CLICOLOR_FORCE")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "0")
	if strings.Contains(run("conversations"), "\x1b") {
		t.Error("CLICOLOR_FORCE=0 is off")
	}
}

func TestTextTruncatesToTerminalWidth(t *testing.T) {
	e := textEnv(t)
	e.sync()
	t.Setenv("COLUMNS", "80")
	_, out, _ := e.run("--format", "text", "--max-age", "0", "unread")
	for _, l := range strings.Split(out, "\n") {
		if l != "" && len([]rune(l)) > 80 {
			t.Errorf("row not clipped: %q", l)
		}
	}
	if !strings.Contains(out, "...") {
		t.Errorf("expected clipped text: %s", out)
	}
}

// jsonNoise is the only run-to-run variance in JSON output.
var jsonNoise = regexp.MustCompile(`"(archive_age_seconds|started_at|finished_at|last_synced_at|last_success_at|finished|started)":("[^"]*"|\d+|null)|\d+(\.\d+)?(ms|s|m)+ ago`)

// TestJSONUnaffectedByTextPath runs every command as JSON with the text path forced on
// (CLICOLOR_FORCE) and off, and requires identical bytes and no escape codes.
func TestJSONUnaffectedByTextPath(t *testing.T) {
	e := textEnv(t)
	e.sync()
	_, out, _ := e.run("--max-age", "0", "search", "Hello")
	first := decode(t, out)["items"].([]any)[0].(map[string]any)
	cmds := [][]string{
		{"doctor"}, {"status"}, {"whoami"}, {"search", "Hello"}, {"messages"}, {"conversations"}, {"people"},
		{"activity"}, {"unread"}, {"sql", "select id from messages order by id limit 3"},
		{"thread", first["conversation_id"].(string), first["id"].(string)},
		{"--fields", "id,text", "--max-text", "9", "messages"},
	}
	for _, c := range cmds {
		args := append([]string{"--max-age", "0", "--json"}, c...)
		t.Setenv("CLICOLOR_FORCE", "")
		_, plain, _ := e.run(args...)
		t.Setenv("CLICOLOR_FORCE", "1")
		_, forced, _ := e.run(args...)
		_, withFlag, _ := e.run(append([]string{"--format", "json"}, args[2:]...)...)
		if strings.Contains(forced, "\x1b") || strings.Contains(plain, "\x1b") {
			t.Errorf("%v: escape in JSON", c)
		}
		p, f, w := jsonNoise.ReplaceAllString(plain, ""), jsonNoise.ReplaceAllString(forced, ""), jsonNoise.ReplaceAllString(withFlag, "")
		if p != f || p != w || !bytes.HasPrefix([]byte(p), []byte("{")) {
			t.Errorf("%v: JSON differs\n%s\n%s", c, p, f)
		}
	}
}
