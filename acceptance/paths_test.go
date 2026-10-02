package acceptance

import (
	"bytes"
	"encoding/binary"
	"os/exec"
	"strings"
	"testing"

	"github.com/ourostack/teamscrawl/internal/v8"
)

func TestFirstDifference(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{`{"properties":{"mentions":[1,2]}}`, `{"properties":{"mentions":[1,3]}}`, "$.properties.mentions[] [value]"},
		{`{"id":1}`, `{"type":1}`, "$.id [key]"},
		{`{"id":[1]}`, `{"id":{"x":1}}`, "$.id [shape]"},
		{`{"id":"1"}`, `{"id":1}`, "$.id [value]"},
		{`{"1234567":{"x":1}}`, `{"1234567":{"x":2}}`, "$.<field>.<field> [value]"},
		{`[1,2]`, `[1]`, "$[] [shape]"},
	}
	for _, c := range cases {
		if got := firstDifference(c.a, c.b); got != c.want {
			t.Errorf("firstDifference(%s, %s) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

// TestDiffScriptAgreesWithGo feeds synthetic payloads through diff.mjs: a decodable value, a
// version 16 relabel, and a payload Node rejects.
func TestDiffScriptAgreesWithGo(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	payloads := [][]byte{
		{0xff, 0x0f, 0x22, 0x02, 'h', 'i'},
		{0xff, 0x10, 0x22, 0x02, 'h', 'i'},
		{0xff, 0x0f, 0x6f, 0x22, 0x01, 'a', 0x49, 0x02, 0x7b, 0x01}, // {a: 1}
		{0xff, 0x0f, 0x5c},                                          // host object: Node rejects it
	}
	var in bytes.Buffer
	for _, p := range payloads {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(p))) //nolint:gosec // tiny test payloads
		in.Write(n[:])
		in.Write(p)
	}
	cmd := exec.Command(node, "diff.mjs") //nolint:gosec // node comes from PATH; the script is this package's own
	cmd.Dir = diffScriptDir(t)
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("%v: %s", err, exitErr.Stderr)
		}
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(payloads) {
		t.Fatalf("got %d lines for %d payloads: %q", len(lines), len(payloads), lines)
	}
	for i, p := range payloads[:3] {
		v, err := v8.Deserialize(p)
		if err != nil {
			t.Fatalf("payload %d: %v", i, err)
		}
		c, _ := v8.Canonical(v)
		if want := "ok " + string(c); lines[i] != want {
			t.Errorf("payload %d: node %q, go %q", i, lines[i], want)
		}
	}
	if !strings.HasPrefix(lines[3], "nc ") {
		t.Errorf("rejected payload: %q", lines[3])
	}
}

func TestSafeKey(t *testing.T) {
	for k, want := range map[string]string{
		"messageMap": "messageMap", "$date": "$date", "version": "version",
		"19:abc@thread.v2": "<field>", "8:orgid:1234": "<field>", "Jane Doe": "<field>", "": "<field>",
	} {
		if got := safeKey(k); got != want {
			t.Errorf("safeKey(%q) = %q, want %q", k, got, want)
		}
	}
}

func TestStderrNoteHidesContent(t *testing.T) {
	stderr := "KeyError: 'private-value'"
	note := stderrNote(t, "tool", []byte(stderr))
	if strings.Contains(note, "private-value") || !strings.Contains(note, "25 bytes") {
		t.Errorf("note leaks or miscounts: %q", note)
	}
}
