package browsertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func dial(t *testing.T, s *Server) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), s.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func roundTrip(t *testing.T, c *websocket.Conn, msg string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestServerDefaultsAndScripts(t *testing.T) {
	s := NewServer(t)
	c := dial(t, s)
	if r := roundTrip(t, c, `{"id":1,"method":"Target.getTargets"}`); r["result"] == nil {
		t.Fatalf("default getTargets: %v", r)
	}
	if r := roundTrip(t, c, `{"id":2,"method":"Nope.nothing","sessionId":"S"}`); r["error"] == nil || r["sessionId"] != "S" {
		t.Fatalf("unknown method must be an error echoing the session: %v", r)
	}
	s.Handle("X.fail", func(Request) (any, *Error) { return nil, &Error{Code: -1, Message: "scripted"} })
	if r := roundTrip(t, c, `{"id":3,"method":"X.fail"}`); r["error"] == nil {
		t.Fatalf("scripted error: %v", r)
	}
	if err := c.Write(context.Background(), websocket.MessageText, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	for i, m := range []string{"Target.createTarget", "Target.attachToTarget", "Page.navigate", "Runtime.evaluate"} {
		if r := roundTrip(t, c, fmt.Sprintf(`{"id":%d,"method":%q}`, 10+i, m)); r["result"] == nil {
			t.Fatalf("default %s: %v", m, r)
		}
	}
	reqs := s.Requests()
	if len(reqs) < 7 || reqs[0].Method != "Target.getTargets" || reqs[1].SessionID != "S" {
		t.Fatalf("recorded requests: %+v", reqs)
	}
	if hs := s.Headers(); len(hs) != 1 || hs[0].Get("Origin") != "" {
		t.Fatalf("headers: %v", hs)
	}
}

func TestServerBrowserCloseCallsOnClose(t *testing.T) {
	s := NewServer(t)
	closed := make(chan struct{})
	s.OnClose(func() { close(closed) })
	c := dial(t, s)
	_ = roundTrip(t, c, `{"id":1,"method":"Browser.close"}`)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("OnClose did not run")
	}
}

func TestServerPushAndDrop(t *testing.T) {
	s := NewServer(t)
	c := dial(t, s)
	_ = roundTrip(t, c, `{"id":1,"method":"Browser.close"}`) // the connection is registered by now
	s.Push(`{"method":"Event"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, data, err := c.Read(ctx); err != nil || !strings.Contains(string(data), "Event") {
		t.Fatalf("push: %s %v", data, err)
	}
	s.DropConnections()
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("the dropped connection must fail to read")
	}
}

func TestServerNotFoundOutsideDevtools(t *testing.T) {
	s := NewServer(t)
	resp, err := http.Get("http://127.0.0.1:" + itoa(s.Port) + "/json/version")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// A plain GET on the websocket path is not a websocket upgrade.
	resp, err = http.Get("http://127.0.0.1:" + itoa(s.Port) + BrowserPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestListenPanicsWithoutLoopback(t *testing.T) {
	old := listen
	t.Cleanup(func() { listen = old })
	listen = func(string, string) (net.Listener, error) { return nil, errors.New("no network") }
	defer func() {
		if recover() == nil {
			t.Fatal("Listen must panic when it cannot listen")
		}
	}()
	Listen()
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestFakeHelpers(t *testing.T) {
	if got := profileArg([]string{"a", "--user-data-dir=/p", "b"}); got != "/p" {
		t.Fatalf("profileArg = %q", got)
	}
	if got := profileArg([]string{"a"}); got != "" {
		t.Fatalf("profileArg = %q", got)
	}
	if got := stdioReport(); got != "null" && got != "not-null" {
		t.Fatalf("stdioReport = %q", got)
	}
	exe := FakeBrowser(t)
	if self, _ := os.Executable(); exe != self || os.Getenv(envFake) != "1" {
		t.Fatalf("FakeBrowser = %q", exe)
	}
	_ = os.Unsetenv(envFake)
	RunIfFake() // not a fake: returns at once
	cmd := exec.Command("true")
	detach(cmd)
	_ = filepath.Join
	ignoreTerm()
}

func TestWriteFailureEndsConnection(t *testing.T) {
	s := NewServer(t)
	dropped := make(chan struct{})
	s.Handle("X.drop", func(Request) (any, *Error) {
		s.DropConnections()
		close(dropped)
		return map[string]any{}, nil
	})
	c := dial(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"X.drop"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("the connection was dropped")
	}
	select {
	case <-dropped:
	case <-ctx.Done():
		t.Fatal("drop handler did not finish closing the connection")
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		remaining := len(s.conns)
		s.mu.Unlock()
		// Removal follows the failed response write, not merely the client observing closure.
		if remaining == 0 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("failed response write did not release the server connection")
		}
	}
}

func TestStdioReportNull(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip("no null device")
	}
	defer func() { _ = null.Close() }()
	in, out, errf := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = null, null, null
	got := stdioReport()
	os.Stdin, os.Stdout, os.Stderr = in, out, errf
	if got != "null" {
		t.Fatalf("stdioReport = %q", got)
	}
}
