package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crawlkit/output"

	"github.com/ourostack/teamscrawl/internal/errs"
)

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC)
	loc := time.FixedZone("x", -7*3600)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-09-01T10:00:00Z", time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)},
		{"2026-09-01T10:00:00+02:00", time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)},
		{"2026-09-01", time.Date(2026, 9, 1, 0, 0, 0, 0, loc)},
		{"90m", now.Add(-90 * time.Minute)},
		{"24h", now.Add(-24 * time.Hour)},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"2w", now.Add(-14 * 24 * time.Hour)},
		{"1h30m", now.Add(-90 * time.Minute)},
	}
	for _, c := range cases {
		got, err := parseWhen(c.in, now, loc)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%q: got %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "yesterday", "-5m", "7x", "2026-13-01", "0"} {
		if _, err := parseWhen(bad, now, loc); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestParseMaxAge(t *testing.T) {
	for in, want := range map[string]time.Duration{"15m": 15 * time.Minute, "0": 0, "1d": 24 * time.Hour, "1ns": time.Nanosecond, "2w": 14 * 24 * time.Hour} {
		got, err := parseMaxAge(in)
		if err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "soon", "-1m"} {
		if _, err := parseMaxAge(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	if s, cut := truncateRunes("héllo wörld", 5); s != "héllo…" || !cut {
		t.Fatalf("%q %v", s, cut)
	}
	if s, cut := truncateRunes("short", 5); s != "short" || cut {
		t.Fatalf("%q %v", s, cut)
	}
	if s, cut := truncateRunes("anything", 0); s != "anything" || cut {
		t.Fatalf("0 disables: %q %v", s, cut)
	}
	if s, _ := truncateRunes("👨‍👩‍👧x", 1); s != "👨…" {
		t.Fatalf("rune based: %q", s)
	}
}

func TestParseDeepLink(t *testing.T) {
	conv, root, err := parseThreadTarget("https://teams.microsoft.com/l/message/19%3Aabc%40thread.tacv2/1700000046000?tenantId=t&parentMessageId=1700000045000&context=%7B%7D", "")
	if err != nil || conv != "19:abc@thread.tacv2" || root != "1700000045000" {
		t.Fatalf("%q %q %v", conv, root, err)
	}
	conv, root, err = parseThreadTarget("https://teams.microsoft.com/l/message/19%3Aabc%40thread.v2/17?tenantId=t", "")
	if err != nil || conv != "19:abc@thread.v2" || root != "17" {
		t.Fatalf("%q %q %v", conv, root, err)
	}
	if _, _, err := parseThreadTarget("https://example.com/x", ""); err == nil {
		t.Fatal("non-Teams link must fail")
	}
	if _, _, err := parseThreadTarget("19:abc@thread.v2", ""); err == nil {
		t.Fatal("conversation without a root must fail")
	}
	conv, root, err = parseThreadTarget("19:abc@thread.v2", "5")
	if err != nil || conv != "19:abc@thread.v2" || root != "5" {
		t.Fatalf("%q %q %v", conv, root, err)
	}
}

func TestParseThreadTargetMalformedLinks(t *testing.T) {
	for _, bad := range []string{
		"https://teams.microsoft.com/l/message/%zz/1",
		"https://teams.microsoft.com/l/message/19%3Aabc/%zz",
		"https://teams.microsoft.com/l/message/",
		"https://teams.microsoft.com/l/message/19%3Aabc",
		"https://teams.microsoft.com/l/message//1",
		"https://teams.microsoft.com/l/chat/19%3Aabc/1",
		"https://evil.example/l/message/19%3Aabc/1",
		"https://[::1",
		"http://%",
	} {
		_, _, err := parseThreadTarget(bad, "")
		var c *errs.Coded
		if !errors.As(err, &c) || c.Code != errs.CodeUsage || !strings.Contains(c.Fix, "https://teams.microsoft.com/l/message/") || !strings.Contains(c.Fix, "thread <conversation-id> <root-message-id>") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestGuessFormatFallsBackToTextOnATerminal(t *testing.T) {
	if got := guessFormat([]string{"search"}, true); got != output.Text {
		t.Fatalf("tty format = %q, want text", got)
	}
	if got := guessFormat([]string{"search"}, false); got != output.JSON {
		t.Fatalf("pipe format = %q, want json", got)
	}
	if got := guessFormat([]string{"--format", "log"}, true); got != output.Log {
		t.Fatalf("explicit format = %q, want log", got)
	}
}

func TestIsTTYMeansACharacterDevice(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	if !isTTY(null) {
		t.Error("a character device must count as a terminal")
	}
	reg, err := os.CreateTemp(t.TempDir(), "f")
	if err != nil {
		t.Fatal(err)
	}
	if isTTY(reg) {
		t.Error("a regular file is not a terminal")
	}
	_ = reg.Close()
	if isTTY(reg) {
		t.Error("a closed file is not a terminal")
	}
	if isTTY(&bytes.Buffer{}) {
		t.Error("a buffer is not a terminal")
	}
}

func TestJSONIsIndentedOnlyOnATerminal(t *testing.T) {
	var out bytes.Buffer
	rt := &runtime{stdout: &out, format: output.JSON, stdoutTTY: true}
	if err := rt.write("x", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\n  \"a\": 1\n}\n" {
		t.Fatalf("tty json = %q", out.String())
	}
	out.Reset()
	rt.stdoutTTY = false
	if err := rt.write("x", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\"a\":1}\n" {
		t.Fatalf("pipe json = %q", out.String())
	}
}

func TestBodyOfAppendsTheDatabaseCause(t *testing.T) {
	b := bodyOf(errs.DBError(errors.New("disk full")))
	if b.Code != errs.CodeDBError || !strings.HasSuffix(b.Message, ": disk full") {
		t.Fatalf("body = %+v", b)
	}
	if got := bodyOf(errs.DBError(nil)).Message; strings.Contains(got, "disk full") || strings.HasSuffix(got, ": ") {
		t.Fatalf("a db error without a cause keeps its plain message: %q", got)
	}
}

func TestBodyOfUsesPlatformPermissionFix(t *testing.T) {
	b := bodyOf(errs.NoFullDiskAccess(`C:\teams`, errors.New("denied")))
	switch goruntime.GOOS {
	case "windows":
		if strings.Contains(b.Fix, "Full Disk Access") || !strings.Contains(b.Message, "Windows denied access") {
			t.Fatalf("body = %+v", b)
		}
	default:
		if !strings.Contains(b.Fix, "Full Disk Access") {
			t.Fatalf("body = %+v", b)
		}
	}
}

func TestWarningsAndErrorsHaveTextAndJSONForms(t *testing.T) {
	c := errs.Usage("bad flag")
	c.Fix = "fix it"
	var out, errb bytes.Buffer
	rt := &runtime{stdout: &out, stderr: &errb, format: output.Text}
	rt.printWarning(c)
	if errb.String() != "warning: bad flag\nfix: fix it\n" {
		t.Fatalf("text warning = %q", errb.String())
	}
	errb.Reset()
	rt.format = output.JSON
	rt.printWarning(c)
	var doc warningDoc
	if err := json.Unmarshal(errb.Bytes(), &doc); err != nil || doc.Warning.Code != errs.CodeUsage || doc.Warning.Fix != "fix it" {
		t.Fatalf("json warning = %q (%v)", errb.String(), err)
	}
}

type fieldedKeys struct {
	Shown   string `json:"shown,omitempty"`
	Skipped string `json:"-"`
	Bare    string
}

func TestJSONKeysSkipsDashAndUntaggedFields(t *testing.T) {
	got := jsonKeys(reflect.TypeFor[fieldedKeys]())
	if len(got) != 1 || got[0] != "shown" {
		t.Fatalf("jsonKeys = %v, want [shown]", got)
	}
}

func TestProjectFailsOnItemsThatAreNotJSONObjects(t *testing.T) {
	if _, err := project(make(chan int), []string{"a"}); err == nil {
		t.Error("an unencodable item must fail")
	}
	if _, err := project("just a string", []string{"a"}); err == nil {
		t.Error("an item that encodes to a non-object must fail")
	}
	p, err := project(map[string]int{"a": 1, "b": 2}, []string{"b", "zzz"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	if string(b) != `{"b":2}` {
		t.Fatalf("projection = %s", b)
	}
}

func TestShapePanicsWhenAnItemCannotBeProjectedAndRunCLIReportsItOnce(t *testing.T) {
	rt := &runtime{fields: []string{"a"}}
	func() {
		defer func() {
			err, ok := recover().(error)
			if !ok || !strings.Contains(err.Error(), "cannot project an item") {
				t.Fatalf("recovered %v, want a projection error", err)
			}
		}()
		shape(rt, []any{make(chan int)})
		t.Fatal("shape did not panic")
	}()

	panicHook = func() { shape(rt, []any{make(chan int)}) }
	t.Cleanup(func() { panicHook = nil })
	var out, errb bytes.Buffer
	code := runCLI(context.Background(), []string{"--json", "version"}, &out, &errb)
	msg, _ := errorOf(t, errb.String())["message"].(string)
	if code != errs.ExitRuntime || strings.Count(msg, "internal") != 1 {
		t.Fatalf("exit %d, message %q: want one internal error that names the cause once", code, msg)
	}
}

func TestShapeKeepsItemsWhole(t *testing.T) {
	in := []map[string]int{{"a": 1, "b": 2}}
	if got := shape(&runtime{}, in); len(got) != 1 || got[0].(map[string]int)["b"] != 2 {
		t.Fatalf("shape without fields = %v", got)
	}
	b, _ := json.Marshal(shape(&runtime{fields: []string{"b"}}, in))
	if string(b) != `[{"b":2}]` {
		t.Fatalf("shape with fields = %s", b)
	}
}

func TestParseDurationRejectsAnOutOfRangeNumber(t *testing.T) {
	huge := strings.Repeat("9", 400) + "d"
	if _, err := parseDuration(huge); err == nil {
		t.Fatal("a number beyond float64 must fail")
	}
	if d, err := parseDuration("1.5d"); err != nil || d != 36*time.Hour {
		t.Fatalf("1.5d = %v, %v", d, err)
	}
}
