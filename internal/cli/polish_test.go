package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchWithoutWordsUsesFilters(t *testing.T) {
	e := newEnv(t)
	e.sync()
	// Each filter alone selects messages; no query word is needed.
	for _, args := range [][]string{{"--mentions-me"}, {"--from", "Pat"}, {"--conversation", "Fixture chat 1"}, {"--since", "2000-01-01"}, {"--until", "2100-01-01"}, {"--team", "Fixture team 1"}} {
		code, stdout, stderr := e.run(append([]string{"--max-age", "0", "search"}, args...)...)
		if code != 0 || len(items(t, decode(t, stdout))) == 0 {
			t.Errorf("search %v: exit %d %s %s", args, code, stdout, stderr)
		}
	}
	// Newest first, like any search.
	_, stdout, _ := e.run("--max-age", "0", "search", "--since", "2000-01-01", "--limit", "5")
	its := items(t, decode(t, stdout))
	for i := 1; i < len(its); i++ {
		if its[i-1]["sent_at"].(string) < its[i]["sent_at"].(string) {
			t.Errorf("not newest first: %v then %v", its[i-1]["sent_at"], its[i]["sent_at"])
		}
	}
	// Nothing at all is a usage error whose fix names messages.
	for _, args := range [][]string{{"search"}, {"search", ""}, {"search", "--limit", "3"}} {
		code, _, stderr := e.run(append([]string{"--max-age", "0"}, args...)...)
		body := errorOf(t, stderr)
		if code != 2 || !strings.Contains(body["fix"].(string), "teamscrawl messages") {
			t.Errorf("%v: exit %d %v", args, code, body)
		}
	}
	// A query still works and combines with a filter: every item matches both.
	_, stdout, _ = e.run("--max-age", "0", "search", "Fixture", "--limit", "500")
	byQuery := items(t, decode(t, stdout))
	_, stdout, _ = e.run("--max-age", "0", "search", "Fixture", "--from", "Alex", "--limit", "500")
	both := items(t, decode(t, stdout))
	if len(both) == 0 || len(both) > len(byQuery) {
		t.Fatalf("query alone found %d, with --from %d", len(byQuery), len(both))
	}
	for _, it := range both {
		text, _ := it["text"].(string)
		sender, _ := it["sender_name"].(string)
		if !strings.Contains(strings.ToLower(text), "fixture") || !strings.Contains(strings.ToLower(sender), "alex") {
			t.Errorf("item matches only one of query and --from: sender %q text %q", sender, text)
		}
	}
	_, stdout, _ = e.run("--max-age", "0", "search", "Fixture", "--from", "Nobody Atall")
	if n := len(items(t, decode(t, stdout))); n != 0 {
		t.Errorf("--from with no such sender still found %d", n)
	}
}

func TestTeamFlagOnEveryCommand(t *testing.T) {
	e := newEnv(t)
	e.sync()
	cases := [][]string{
		{"messages"}, {"search", "Fixture"}, {"unread", "--include-channels"}, {"unread", "--by-conversation", "--include-channels"},
		{"conversations"}, {"activity"},
	}
	for _, base := range cases {
		code, stdout, stderr := e.run(append(append([]string{"--max-age", "0"}, base...), "--team", "fixture TEAM 1")...)
		if code != 0 {
			t.Errorf("%v: exit %d %s", base, code, stderr)
			continue
		}
		for _, it := range items(t, decode(t, stdout)) {
			if name, _ := it["conversation_display_name"].(string); name != "" && !strings.HasPrefix(name, "Fixture team 1") {
				t.Errorf("%v: item outside the team: %v", base, name)
			}
		}
	}
	// The team's channels are all there, the other team's are not.
	_, stdout, _ := e.run("--max-age", "0", "conversations", "--team", "Fixture team 1")
	if n := len(items(t, decode(t, stdout))); n < 3 {
		t.Errorf("conversations --team: %d items, want the team and its channels", n)
	}
	// An id works too.
	_, stdout, _ = e.run("--max-age", "0", "conversations", "--team", "Fixture team 1")
	teamID := ""
	for _, it := range items(t, decode(t, stdout)) {
		if it["kind"] == "Space" {
			teamID = it["id"].(string)
		}
	}
	code, stdout, _ := e.run("--max-age", "0", "conversations", "--team", teamID)
	if code != 0 || len(items(t, decode(t, stdout))) < 3 {
		t.Errorf("by id %q: exit %d %s", teamID, code, stdout)
	}
	// Unknown team: usage error with a way forward.
	code, _, stderr := e.run("--max-age", "0", "messages", "--team", "No such team")
	body := errorOf(t, stderr)
	if code != 2 || body["code"] != "usage" || !strings.Contains(body["fix"].(string), "teamscrawl teams") {
		t.Errorf("unknown team: exit %d %v", code, body)
	}
}

func TestTotalAppearsOnlyWhenTruncated(t *testing.T) {
	e := newEnv(t)
	e.sync()
	for _, args := range [][]string{
		{"search", "Fixture"}, {"messages"}, {"unread", "--include-channels"}, {"unread", "--by-conversation", "--include-channels"},
		{"conversations"}, {"people"}, {"activity"}, {"thread", "19:topicchannel1@thread.tacv2", "1700000045000"},
	} {
		code, stdout, stderr := e.run(append(append([]string{"--max-age", "0"}, args...), "--limit", "1")...)
		m := decode(t, stdout)
		if code != 0 {
			t.Errorf("%v: exit %d %s", args, code, stderr)
			continue
		}
		all := e.runCount(append(append([]string{"--max-age", "0"}, args...), "--limit", "500"))
		if m["truncated"] == true {
			if got := int(m["total"].(float64)); got != all || got < 2 {
				t.Errorf("%v: total %v, want %d", args, m["total"], all)
			}
		} else if _, has := m["total"]; has {
			t.Errorf("%v: total present without truncation: %v", args, m)
		}
	}
	// Not truncated: no total key.
	_, stdout, _ := e.run("--max-age", "0", "search", "Fixture", "--limit", "500")
	if _, has := decode(t, stdout)["total"]; has {
		t.Error("total on a complete list")
	}
	// sql stops reading at the limit, so it cannot know the total.
	_, stdout, _ = e.run("--max-age", "0", "sql", "select id from messages", "--limit", "2")
	m := decode(t, stdout)
	if _, has := m["total"]; m["truncated"] != true || has {
		t.Errorf("sql: %v", m)
	}
	_, stdout, _ = e.run("--max-age", "0", "sql", "select 1", "--limit", "2")
	if _, has := decode(t, stdout)["total"]; has {
		t.Error("sql total on a complete result")
	}
}

// runCount returns how many items a list command returns.
func (e *env) runCount(args []string) int {
	e.t.Helper()
	code, stdout, stderr := e.run(args...)
	if code != 0 {
		e.t.Fatalf("%v: exit %d %s", args, code, stderr)
	}
	return len(items(e.t, decode(e.t, stdout)))
}

func TestTotalInTextFooter(t *testing.T) {
	res := newList(nil, true).withTotal(312)
	if got := renderToString(t, "search", res); !strings.Contains(got, "0 of 312 items (more exist; raise --limit)") {
		t.Errorf("footer:\n%s", got)
	}
	if got := renderToString(t, "search", newList(nil, false).withTotal(5)); strings.Contains(got, " of 5") {
		t.Errorf("total shown without truncation:\n%s", got)
	}
}

func TestReplyCountAndCardTextInOutput(t *testing.T) {
	e := newEnv(t)
	e.sync()
	_, stdout, _ := e.run("--max-age", "0", "messages", "-c", "Fixture team 1 › General", "--limit", "500")
	var root map[string]any
	for _, it := range items(t, decode(t, stdout)) {
		if strings.HasPrefix(it["text"].(string), "Channel post with a subject") && it["id"] == it["reply_chain_id"] {
			root = it
		}
	}
	if root == nil || root["reply_count"] != float64(1) || root["last_reply_at"] == nil {
		t.Fatalf("root: %v", root)
	}
	// A reply and a chat message carry no reply_count.
	_, stdout, _ = e.run("--max-age", "0", "messages", "-c", "Fixture chat 1", "--limit", "500")
	var sawCard, sawCall, sawMember bool
	for _, it := range items(t, decode(t, stdout)) {
		if _, has := it["reply_count"]; has {
			t.Errorf("chat message has reply_count: %v", it)
		}
		switch it["text"] {
		case "Adaptive fixture card":
			sawCard = true
		case "Call ended":
			sawCall = true
		case "Member added":
			sawMember = true
		}
	}
	if !sawCard || !sawCall || !sawMember {
		t.Errorf("card %v, call %v, member %v", sawCard, sawCall, sawMember)
	}
	_, stdout, _ = e.run("--max-age", "0", "--fields", "id,reply_count", "messages", "-c", "Fixture team 1 › General")
	if !strings.Contains(stdout, "reply_count") {
		t.Errorf("--fields reply_count: %s", stdout)
	}
}

func TestActivityTypeListFlag(t *testing.T) {
	e := newEnv(t)
	e.sync()
	// A list that names no type is a usage error, not "every type".
	for _, typ := range []string{",", " , ,"} {
		code, _, stderr := e.run("--max-age", "0", "activity", "--type", typ)
		if body := errorOf(t, stderr); code != 2 || !strings.Contains(body["message"].(string), "--type") {
			t.Errorf("--type %q: exit %d %v", typ, code, body)
		}
	}
	n := func(typ string) int {
		return e.runCount([]string{"--max-age", "0", "activity", "--type", typ})
	}
	if a, b, both := n("mention"), n("follow"), n("mention,follow"); a+b != both || both == 0 {
		t.Errorf("type list: %d + %d != %d", a, b, both)
	}
	_, stdout, stderr := e.run("activity", "--help")
	out := strings.Join(strings.Fields(stdout+stderr), " ")
	for _, w := range []string{"mention", "mentionInChat", "replyToReply", "reactionInChat", "msGraph", "comma separated"} {
		if !strings.Contains(out, w) {
			t.Errorf("activity --help lacks %q", w)
		}
	}
}

func TestConversationsQueryHelpDocumentsRanking(t *testing.T) {
	e := newEnv(t)
	_, stdout, stderr := e.run("conversations", "--help")
	out := strings.Join(strings.Fields(stdout+stderr), " ")
	for _, w := range []string{"exact name, then names that start with the query, then names that contain it", "--team"} {
		if !strings.Contains(out, w) {
			t.Errorf("conversations --help lacks %q", w)
		}
	}
}

func TestRelativeDBPath(t *testing.T) {
	e := newEnv(t)
	t.Chdir(t.TempDir())
	e.db = filepath.Join("data", "rel.db")
	e.sync()
	code, stdout, stderr := e.run("--max-age", "0", "messages", "--limit", "1")
	if code != 0 || len(items(t, decode(t, stdout))) != 1 {
		t.Fatalf("relative --db: exit %d %s %s", code, stdout, stderr)
	}
}
