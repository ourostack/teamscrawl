package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ourostack/teamscrawl/internal/errs"
	"github.com/ourostack/teamscrawl/internal/teamsdesktop"
)

func person(a teamsdesktop.Account, id, name string) teamsdesktop.Person {
	return teamsdesktop.Person{TenantID: a.TenantID, ID: id, DisplayName: name, SeenAt: base}
}

func TestSenderNameFallsBackToPeople(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	m := msg(acctA, "c", "m1", "Call ended", base)
	m.SenderID, m.SenderName = "8:orgid:ana", ""
	named := msg(acctA, "c", "m2", "hi", base.Add(time.Minute))
	named.SenderID, named.SenderName = "8:orgid:ben", "Ben In Message"
	stranger := msg(acctA, "c", "m3", "yo", base.Add(2*time.Minute))
	stranger.SenderID, stranger.SenderName = "8:orgid:nobody", ""
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{m, named, stranger}))
	must(s.ApplyPeople(ctx, []teamsdesktop.Person{person(acctA, "8:orgid:ana", "Ana Person"), person(acctA, "8:orgid:ben", "Ben Person")}))
	rows, _ := must2(s.Messages(ctx, Filter{}))
	got := []string{rows[0].SenderName, rows[1].SenderName, rows[2].SenderName}
	if !eqStrings(got, []string{"Ana Person", "Ben In Message", ""}) {
		t.Fatalf("sender names: %q", got)
	}
	// --from matches the resolved name too.
	rows, _ = must2(s.Messages(ctx, Filter{From: "ana person"}))
	if !eqStrings(ids(rows), []string{"m1"}) {
		t.Fatalf("from by resolved name: %v", ids(rows))
	}
	// An activity item's sender resolves the same way.
	must(s.ApplyActivity(ctx, []teamsdesktop.Activity{{TenantID: acctA.TenantID, UserID: acctA.UserID, ID: "a1", Type: "mention", At: base, ConversationID: "c", MessageID: "m1"}}))
	act, _ := must2(s.Activity(ctx, ActivityFilter{}))
	if len(act) != 1 || act[0].SenderName != "Ana Person" {
		t.Fatalf("activity sender: %+v", act)
	}
}

func TestReplyCountOnChannelRoots(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	team := conv(acctA, "19:team@thread.tacv2", "Space", "Team")
	ch := conv(acctA, "19:chan@thread.tacv2", "Topic", "Chan")
	ch.TeamID = team.ID
	chat := conv(acctA, "19:chat@thread.v2", "Chat", "Chat")
	must(s.ApplyConversations(ctx, []teamsdesktop.Conversation{team, ch, chat}))
	post := func(c, id, text string, at time.Time, root string) teamsdesktop.Message {
		m := msg(acctA, c, id, text, at)
		m.ReplyChainID = root
		if root != id {
			m.ParentMessageID = root
		}
		return m
	}
	gone := post(ch.ID, "r3", "deleted reply", base.Add(3*time.Minute), "root")
	gone.DeletedAt = base.Add(4 * time.Minute)
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{
		post(ch.ID, "root", "the root", base, "root"),
		post(ch.ID, "r1", "first reply", base.Add(time.Minute), "root"),
		post(ch.ID, "r2", "second reply", base.Add(2*time.Minute), "root"),
		gone,
		post(ch.ID, "lonely", "no replies yet", base.Add(5*time.Minute), "lonely"),
		post(chat.ID, "c1", "chat root", base, "c1"),
		post(chat.ID, "c2", "chat reply", base.Add(time.Minute), "c1"),
	}))
	byID := func(rows []MessageRow) map[string]MessageRow {
		out := map[string]MessageRow{}
		for _, r := range rows {
			out[r.ID] = r
		}
		return out
	}
	rows, _ := must2(s.Messages(ctx, Filter{}))
	m := byID(rows)
	if r := m["root"]; r.ReplyCount == nil || *r.ReplyCount != 2 || !r.LastReplyAt.Equal(base.Add(2*time.Minute)) {
		t.Errorf("root: %+v", r)
	}
	if r := m["lonely"]; r.ReplyCount == nil || *r.ReplyCount != 0 || !r.LastReplyAt.IsZero() {
		t.Errorf("root without replies: count %v last %v", r.ReplyCount, r.LastReplyAt)
	}
	for _, id := range []string{"r1", "r2", "c1", "c2"} {
		if m[id].ReplyCount != nil || !m[id].LastReplyAt.IsZero() {
			t.Errorf("%s must not carry a reply count: %+v", id, m[id])
		}
	}
	// search and thread carry it too.
	hits, _ := must2(s.Search(ctx, `"the root"`, Filter{}))
	if len(hits) != 1 || hits[0].ReplyCount == nil || *hits[0].ReplyCount != 2 {
		t.Errorf("search: %+v", hits)
	}
	th, _ := must2(s.Thread(ctx, ch.ID, "root", Filter{}))
	if len(th) != 3 || th[0].ReplyCount == nil || *th[0].ReplyCount != 2 || th[1].ReplyCount != nil {
		t.Errorf("thread: %+v", th)
	}
}

// seedTeams stores two teams named Alpha and Beta, a second team also named Alpha, their
// channels with one message each, a chat, and an unread chat message.
func seedTeams(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	mk := func(id, kind, name, team string) teamsdesktop.Conversation {
		c := conv(acctA, id, kind, name)
		c.TeamID = team
		c.LastMessageAt = base
		return c
	}
	chat := mk("19:chat@thread.v2", "Chat", "Alpha chat", "19:chat@thread.v2")
	chat.ReadHorizonAt = base.Add(-time.Hour)
	must(s.ApplyConversations(ctx, []teamsdesktop.Conversation{
		mk("19:alpha@thread.tacv2", "Space", "Alpha", "19:alpha@thread.tacv2"),
		mk("19:alphachan@thread.tacv2", "Topic", "General", "19:alpha@thread.tacv2"),
		mk("19:beta@thread.tacv2", "Space", "Beta", "19:beta@thread.tacv2"),
		mk("19:betachan@thread.tacv2", "Topic", "Planning", "19:beta@thread.tacv2"),
		mk("19:alpha2@thread.tacv2", "Space", "alpha", "19:alpha2@thread.tacv2"),
		mk("19:alpha2chan@thread.tacv2", "Topic", "Other", "19:alpha2@thread.tacv2"),
		chat,
	}))
	msgs := []teamsdesktop.Message{
		msg(acctA, "19:alphachan@thread.tacv2", "a1", "alpha post", base),
		msg(acctA, "19:betachan@thread.tacv2", "b1", "beta post", base.Add(time.Minute)),
		msg(acctA, "19:alpha2chan@thread.tacv2", "x1", "other alpha post", base.Add(2*time.Minute)),
		msg(acctA, "19:chat@thread.v2", "k1", "chat post", base.Add(3*time.Minute)),
	}
	must(s.ApplyMessages(ctx, msgs))
	must(s.ApplyActivity(ctx, []teamsdesktop.Activity{
		{TenantID: acctA.TenantID, UserID: acctA.UserID, ID: "act-b", Type: "mention", At: base, ConversationID: "19:betachan@thread.tacv2", MessageID: "b1"},
		{TenantID: acctA.TenantID, UserID: acctA.UserID, ID: "act-k", Type: "mention", At: base, ConversationID: "19:chat@thread.v2", MessageID: "k1"},
	}))
}

func TestTeamFilter(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedTeams(t, s)
	// By id, and by case-insensitive exact name ("Beta" is unique).
	for _, team := range []string{"19:beta@thread.tacv2", "Beta", "bETA"} {
		got, _ := must2(s.Messages(ctx, Filter{Team: team}))
		if !eqStrings(ids(got), []string{"b1"}) {
			t.Errorf("messages --team %q: %v", team, ids(got))
		}
	}
	hits, _ := must2(s.Search(ctx, "post", Filter{Team: "Beta"}))
	if !eqStrings(ids(hits), []string{"b1"}) {
		t.Errorf("search: %v", ids(hits))
	}
	convs, _ := must2(s.Conversations(ctx, "", "", Filter{Team: "Beta"}))
	if len(convs) != 2 {
		t.Errorf("conversations: %d, want the team and its channel", len(convs))
	}
	act, _ := must2(s.Activity(ctx, ActivityFilter{Team: "Beta"}))
	if len(act) != 1 || act[0].ID != "act-b" {
		t.Errorf("activity: %+v", act)
	}
	// unread: the chat is unread but is not in the team; the channel is excluded by default.
	un, _ := must2(s.Unread(ctx, Filter{Team: "Beta", IncludeChannels: true}))
	if len(un) != 0 {
		t.Errorf("unread leaks across teams: %v", ids(un))
	}
	// A duplicate name is ambiguous and lists every match.
	_, _, err := s.Messages(ctx, Filter{Team: "Alpha"})
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.CodeUsage {
		t.Fatalf("ambiguous: %v", err)
	}
	if !strings.Contains(coded.Message+coded.Fix, "19:alpha@thread.tacv2") || !strings.Contains(coded.Message+coded.Fix, "19:alpha2@thread.tacv2") {
		t.Errorf("ambiguity must list the matches: %q / %q", coded.Message, coded.Fix)
	}
	// No match: usage error with a fix that says how to find a team.
	_, _, err = s.Messages(ctx, Filter{Team: "Nope"})
	if !errors.As(err, &coded) || coded.Code != errs.CodeUsage || !strings.Contains(coded.Fix, "teamscrawl teams") {
		t.Fatalf("no match: %v", err)
	}
	// A chat is not a team: its name does not resolve.
	if _, _, err = s.Messages(ctx, Filter{Team: "Alpha chat"}); err == nil {
		t.Error("a chat name must not resolve as a team")
	}
	// The account filter scopes the match.
	if _, _, err = s.Messages(ctx, Filter{Team: "Beta", Account: &acctB}); err == nil {
		t.Error("another account's teams must not resolve")
	}
	got, _ := must2(s.Messages(ctx, Filter{Team: "Beta", Account: &acctA}))
	if len(got) != 1 {
		t.Errorf("scoped: %v", ids(got))
	}
}

func TestTotalWhenTruncated(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	var ms []teamsdesktop.Message
	for i := range 5 {
		m := msg(acctA, "19:c@thread.v2", fmt.Sprintf("m%d", i), "needle", base.Add(time.Duration(i)*time.Minute))
		ms = append(ms, m)
	}
	c := conv(acctA, "19:c@thread.v2", "Chat", "One")
	c.ReadHorizonAt = base.Add(-time.Hour)
	var convs []teamsdesktop.Conversation
	convs = append(convs, c)
	for i := range 4 {
		o := conv(acctA, fmt.Sprintf("19:o%d@thread.v2", i), "Chat", fmt.Sprintf("Other %d", i))
		o.ReadHorizonAt = base.Add(-time.Hour)
		convs = append(convs, o)
		m := msg(acctA, o.ID, fmt.Sprintf("o%d", i), "needle", base.Add(time.Duration(i)*time.Minute))
		ms = append(ms, m)
	}
	must(s.ApplyConversations(ctx, convs))
	must(s.ApplyMessages(ctx, ms))
	var peeps []teamsdesktop.Person
	for i := range 6 {
		peeps = append(peeps, person(acctA, fmt.Sprintf("8:orgid:p%d", i), fmt.Sprintf("Person %d", i)))
	}
	must(s.ApplyPeople(ctx, peeps))
	var acts []teamsdesktop.Activity
	for i := range 4 {
		acts = append(acts, teamsdesktop.Activity{TenantID: acctA.TenantID, UserID: acctA.UserID, ID: fmt.Sprintf("a%d", i), Type: "mention", At: base.Add(time.Duration(i) * time.Minute)})
	}
	must(s.ApplyActivity(ctx, acts))

	type listFn func(total *int) (n int, truncated bool, err error)
	lists := map[string]struct {
		run   listFn
		total int
	}{
		"Messages": {func(tot *int) (int, bool, error) {
			r, tr, err := s.Messages(ctx, Filter{Limit: 2, Total: tot})
			return len(r), tr, err
		}, 9},
		"Search": {func(tot *int) (int, bool, error) {
			r, tr, err := s.Search(ctx, "needle", Filter{Limit: 2, Total: tot})
			return len(r), tr, err
		}, 9},
		// Filters that read the joined conversation, people and team rows keep those joins in the count.
		"Messages by conversation name": {func(tot *int) (int, bool, error) {
			r, tr, err := s.Messages(ctx, Filter{Conversation: "One", Limit: 2, Total: tot})
			return len(r), tr, err
		}, 5},
		"Messages by sender name": {func(tot *int) (int, bool, error) {
			r, tr, err := s.Messages(ctx, Filter{From: "Sender", Limit: 2, Total: tot})
			return len(r), tr, err
		}, 9},
		"Unread": {func(tot *int) (int, bool, error) {
			r, tr, err := s.Unread(ctx, Filter{Limit: 2, Total: tot})
			return len(r), tr, err
		}, 9},
		"UnreadByConversation": {func(tot *int) (int, bool, error) {
			r, tr, err := s.UnreadByConversation(ctx, Filter{Limit: 2, Total: tot})
			return len(r), tr, err
		}, 5},
		"Thread": {func(tot *int) (int, bool, error) {
			r, tr, err := s.Thread(ctx, "19:c@thread.v2", "m0", Filter{Limit: 1, Total: tot})
			return len(r), tr, err
		}, 5},
		"Conversations": {func(tot *int) (int, bool, error) {
			r, tr, err := s.Conversations(ctx, "", "", Filter{Limit: 2, Total: tot})
			return len(r), tr, err
		}, 5},
		"People": {func(tot *int) (int, bool, error) {
			r, tr, err := s.People(ctx, "", Filter{Limit: 2, Total: tot})
			return len(r), tr, err
		}, 6},
		"Activity": {func(tot *int) (int, bool, error) {
			r, tr, err := s.Activity(ctx, ActivityFilter{Limit: 2, Total: tot})
			return len(r), tr, err
		}, 4},
	}
	// Thread filters by id on its own: every message of the chat chain is the same thread only
	// when it shares the root, so seed replies for it.
	var replies []teamsdesktop.Message
	for i := 1; i < 5; i++ {
		r := ms[i]
		r.ReplyChainID, r.ParentMessageID = "m0", "m0"
		replies = append(replies, r)
	}
	must(s.ApplyMessages(ctx, replies))
	for name, l := range lists {
		t.Run(name, func(t *testing.T) {
			total := -1
			n, trunc, err := l.run(&total)
			if err != nil || !trunc || total != l.total || n == 0 {
				t.Fatalf("truncated: n=%d trunc=%v total=%d (want %d) err=%v", n, trunc, total, l.total, err)
			}
			// Dropping the joins the filter never reads must not change the count.
			pruneJoinsOff = true
			unpruned := -1
			_, _, err = l.run(&unpruned)
			pruneJoinsOff = false
			if err != nil || unpruned != total {
				t.Fatalf("pruned count %d, full-join count %d (%v)", total, unpruned, err)
			}
			// Not truncated: the total is left alone (the caller omits it).
			total = -1
			if _, _, err := runUntruncated(ctx, s, name, &total); err != nil || total != -1 {
				t.Fatalf("not truncated: total=%d err=%v", total, err)
			}
			// A nil Total is fine.
			if _, _, err := l.run(nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// runUntruncated runs the named list with a limit large enough that nothing is cut.
func runUntruncated(ctx context.Context, s *Store, name string, total *int) (int, bool, error) {
	f := Filter{Limit: 100, Total: total}
	switch name {
	case "Messages", "Messages by conversation name", "Messages by sender name":
		r, tr, err := s.Messages(ctx, f)
		return len(r), tr, err
	case "Search":
		r, tr, err := s.Search(ctx, "needle", f)
		return len(r), tr, err
	case "Unread":
		r, tr, err := s.Unread(ctx, f)
		return len(r), tr, err
	case "UnreadByConversation":
		r, tr, err := s.UnreadByConversation(ctx, f)
		return len(r), tr, err
	case "Thread":
		r, tr, err := s.Thread(ctx, "19:c@thread.v2", "m0", f)
		return len(r), tr, err
	case "Conversations":
		r, tr, err := s.Conversations(ctx, "", "", f)
		return len(r), tr, err
	case "People":
		r, tr, err := s.People(ctx, "", f)
		return len(r), tr, err
	}
	r, tr, err := s.Activity(ctx, ActivityFilter{Limit: 100, Total: total})
	return len(r), tr, err
}

func TestUntitledChatNamesFromPeople(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	me := selfMRI(acctA)
	mk := func(id, kind string, members ...string) teamsdesktop.Conversation {
		c := conv(acctA, id, kind, "")
		c.Members = members
		c.LastMessageAt = base
		return c
	}
	must(s.ApplyConversations(ctx, []teamsdesktop.Conversation{
		mk("19:five@thread.v2", "Chat", me, "8:orgid:1", "8:orgid:2", "8:orgid:3", "8:orgid:4", "8:orgid:5"),
		mk("19:partial@thread.v2", "Chat", me, "8:orgid:1", "8:orgid:ghost"),
		mk("19:nobody@thread.v2", "Chat", me, "8:orgid:ghost", "8:orgid:ghost2"),
		mk("19:nobody2@thread.v2", "Chat", me, "8:orgid:ghost", "8:orgid:ghost3"),
		mk("19:alone@thread.v2", "Chat"),
		mk("19:odd@thread.v2", "Thread", "8:orgid:ghost"),
		conv(acctA, "19:titled@thread.v2", "Chat", "Titled chat"),
	}))
	must(s.ApplyPeople(ctx, []teamsdesktop.Person{
		person(acctA, me, "Me Myself"),
		person(acctA, "8:orgid:1", "Ana"), person(acctA, "8:orgid:2", "Ben"), person(acctA, "8:orgid:3", "Chao"),
		person(acctA, "8:orgid:4", "Dee"), person(acctA, "8:orgid:5", "Eli"),
	}))
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{msg(acctA, "19:five@thread.v2", "m1", "hello", base), msg(acctA, "19:orphan@thread.v2", "m2", "orphan", base)}))
	must(s.ApplyActivity(ctx, []teamsdesktop.Activity{{TenantID: acctA.TenantID, UserID: acctA.UserID, ID: "a1", Type: "mention", At: base, ConversationID: "19:five@thread.v2", MessageID: "m1"}}))

	rows, _ := must2(s.Conversations(ctx, "", "", Filter{}))
	names := map[string]string{}
	for _, r := range rows {
		names[r.ID] = r.DisplayName
	}
	want := map[string]string{
		"19:five@thread.v2":    "Ana, Ben, Chao +2",
		"19:partial@thread.v2": "Ana +1",
		"19:titled@thread.v2":  "Titled chat",
	}
	for id, w := range want {
		if names[id] != w {
			t.Errorf("%s: %q, want %q", id, names[id], w)
		}
	}
	// Nobody known: a generic name that still tells chats apart (member count and id), never "".
	for _, id := range []string{"19:nobody@thread.v2", "19:nobody2@thread.v2", "19:alone@thread.v2", "19:odd@thread.v2"} {
		if !strings.HasPrefix(names[id], "Unnamed ") {
			t.Errorf("%s: %q", id, names[id])
		}
	}
	if names["19:nobody@thread.v2"] == names["19:nobody2@thread.v2"] {
		t.Errorf("two untitled chats share a name: %q", names["19:nobody@thread.v2"])
	}
	if !strings.Contains(names["19:nobody@thread.v2"], "3 members") || !strings.Contains(names["19:alone@thread.v2"], "alone") || !strings.HasPrefix(names["19:odd@thread.v2"], "Unnamed conversation") {
		t.Errorf("generic names: %q %q %q", names["19:nobody@thread.v2"], names["19:alone@thread.v2"], names["19:odd@thread.v2"])
	}
	// Messages, activity and unread overviews resolve the same name; a message in a conversation
	// that was never archived stays blank (there is nothing to name it from).
	ms, _ := must2(s.Messages(ctx, Filter{}))
	for _, m := range ms {
		switch m.ConversationID {
		case "19:five@thread.v2":
			if m.ConversationDisplayName != "Ana, Ben, Chao +2" {
				t.Errorf("message conversation name: %q", m.ConversationDisplayName)
			}
		case "19:orphan@thread.v2":
			if m.ConversationDisplayName != "" {
				t.Errorf("orphan: %q", m.ConversationDisplayName)
			}
		}
	}
	act, _ := must2(s.Activity(ctx, ActivityFilter{}))
	if len(act) != 1 || act[0].ConversationDisplayName != "Ana, Ben, Chao +2" {
		t.Errorf("activity: %+v", act)
	}
	cv := conv(acctA, "19:five@thread.v2", "Chat", "")
	cv.Members, cv.ReadHorizonAt = []string{me, "8:orgid:1"}, base.Add(-time.Hour)
	must(s.ApplyConversations(ctx, []teamsdesktop.Conversation{cv}))
	un, _ := must2(s.UnreadByConversation(ctx, Filter{}))
	if len(un) != 1 || un[0].DisplayName != "Ana" {
		t.Errorf("unread overview: %+v", un)
	}
}

func TestConversationsQueryRanking(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	names := []struct{ id, name string }{
		{"c1", "Weekly sync notes"},  // substring
		{"c2", "Sync"},               // exact
		{"c3", "Syncing the world"},  // prefix
		{"c4", "Team standup"},       // no match
		{"c5", "Daily sync"},         // substring
		{"c6", "sync up"},            // prefix (case-insensitive)
		{"c7", "Resync"},             // substring inside a word: FTS would miss it
		{"c8", "Planning and syncs"}, // substring
	}
	// A channel named exactly "Sync" shows as "Platform \u203a Sync": its own name is an exact match.
	team := conv(acctA, "tm", "Space", "Platform")
	chn := conv(acctA, "c9", "Topic", "Sync")
	chn.TeamID, chn.LastMessageAt = "tm", base.Add(8*time.Minute)
	cs := []teamsdesktop.Conversation{team, chn}
	for i, n := range names {
		c := conv(acctA, n.id, "Chat", n.name)
		c.LastMessageAt = base.Add(time.Duration(i) * time.Minute) // later index = newer
		cs = append(cs, c)
	}
	must(s.ApplyConversations(ctx, cs))
	rows, trunc := must2(s.Conversations(ctx, "", "sync", Filter{}))
	if trunc {
		t.Fatal("not truncated")
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.ID)
	}
	// exact (newest first), then prefix (newest first), then substring (newest first).
	if want := []string{"c9", "c2", "c6", "c3", "c8", "c7", "c5", "c1"}; !eqStrings(got, want) {
		t.Fatalf("ranking: %v, want %v", got, want)
	}
	// Several words still match as words, after name matches.
	must(s.ApplyConversations(ctx, []teamsdesktop.Conversation{conv(acctA, "c10", "Chat", "Notes: weekly planning")}))
	rows, _ = must2(s.Conversations(ctx, "", "weekly notes", Filter{}))
	if len(rows) == 0 || rows[0].ID != "c1" {
		t.Fatalf("multi-word: %+v", rows)
	}
	// Wildcards in the query are literal.
	if rows, _ = must2(s.Conversations(ctx, "", "100%", Filter{})); len(rows) != 0 {
		t.Fatalf("percent is literal: %+v", rows)
	}
	// A query with nothing searchable is still a usage error.
	if _, _, err := s.Conversations(ctx, "", `"`, Filter{}); err == nil {
		t.Fatal("empty query terms must be an error")
	}
	// The ranking holds with a limit (truncation cuts the tail, not the best match).
	rows, trunc = must2(s.Conversations(ctx, "", "sync", Filter{Limit: 2}))
	if len(rows) != 2 || !trunc || rows[0].ID != "c9" || rows[1].ID != "c2" {
		t.Fatalf("limit: %+v %v", rows, trunc)
	}
}

func TestSearchFiltersOnly(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := msg(acctA, "c", "m1", "alpha", base)
	a.SenderID, a.SenderName, a.MentionsMe = "8:orgid:ana", "Ana", true
	b := msg(acctA, "c", "m2", "beta", base.Add(time.Minute))
	b.SenderID, b.SenderName = "8:orgid:ben", "Ben"
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{a, b}))
	for name, f := range map[string]Filter{
		"mentions-me":  {MentionsMe: true},
		"from":         {From: "Ana"},
		"conversation": {Conversation: "c"},
		"since":        {Since: base.Add(-time.Hour)},
		"until":        {Until: base.Add(time.Hour)},
	} {
		rows, _, err := s.Search(ctx, "  ", f)
		if err != nil || len(rows) == 0 {
			t.Errorf("%s: %v %v", name, ids(rows), err)
		}
	}
	// Newest first, like every search.
	rows, _ := must2(s.Search(ctx, "", Filter{Since: base.Add(-time.Hour)}))
	if !eqStrings(ids(rows), []string{"m2", "m1"}) {
		t.Errorf("order: %v", ids(rows))
	}
	// A query with a filter still searches text.
	rows, _ = must2(s.Search(ctx, "alpha", Filter{Since: base.Add(-time.Hour)}))
	if !eqStrings(ids(rows), []string{"m1"}) {
		t.Errorf("query and filter: %v", ids(rows))
	}
	// Nothing at all: a usage error whose fix names messages.
	for _, f := range []Filter{{}, {Limit: 5}, {IncludeDeleted: true}, {IncludeSystem: true}, {Account: &acctA}} {
		_, _, err := s.Search(ctx, "", f)
		var coded *errs.Coded
		if !errors.As(err, &coded) || coded.Code != errs.CodeUsage || !strings.Contains(coded.Fix, "messages") {
			t.Errorf("empty search %+v: %v", f, err)
		}
	}
	// A query of only punctuation, with a filter, is filters-only; without one it is an error.
	if _, _, err := s.Search(ctx, "*", Filter{Account: &acctA}); err == nil {
		t.Error("unsearchable query without filters must fail")
	}
}

func TestActivityTypeList(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	var acts []teamsdesktop.Activity
	for i, ty := range []string{"mention", "mentionInChat", "reply", "reactionInChat"} {
		acts = append(acts, teamsdesktop.Activity{TenantID: acctA.TenantID, UserID: acctA.UserID, ID: fmt.Sprintf("a%d", i), Type: ty, At: base.Add(time.Duration(i) * time.Minute)})
	}
	must(s.ApplyActivity(ctx, acts))
	count := func(typ string) int {
		rows, _ := must2(s.Activity(ctx, ActivityFilter{Type: typ}))
		return len(rows)
	}
	for _, c := range []struct {
		typ  string
		want int
	}{{"mention", 1}, {"MENTIONINCHAT", 1}, {"mention,mentionInChat", 2}, {" mention , reply ,", 2}, {"mention,nope", 1}, {"", 4}} {
		if got := count(c.typ); got != c.want {
			t.Errorf("type %q: %d, want %d", c.typ, got, c.want)
		}
	}
}

func TestAmbiguousTeamListIsCapped(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	var cs []teamsdesktop.Conversation
	for i := range maxTeamMatches + 2 {
		team := conv(acctA, fmt.Sprintf("19:dup%02d@thread.tacv2", i), "Space", "Dup")
		ch := conv(acctA, fmt.Sprintf("19:dupch%02d@thread.tacv2", i), "Topic", "General")
		ch.TeamID = team.ID
		cs = append(cs, team, ch)
	}
	must(s.ApplyConversations(ctx, cs))
	_, _, err := s.Messages(ctx, Filter{Team: "dup"})
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.CodeUsage || !strings.HasSuffix(coded.Message, ", ...") || strings.Count(coded.Message, "19:dup") != maxTeamMatches {
		t.Fatalf("capped list: %v", err)
	}
}

func TestOpenReadOnlyAcceptsRelativePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rel := filepath.Join("data", "rel.db")
	s, err := Open(ctx, filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{msg(acctA, "c", "m1", "hello", base)}))
	_ = s.Close()
	t.Chdir(dir)
	rw, err := Open(ctx, rel) // a relative path opens the writer too
	if err != nil {
		t.Fatalf("relative path, writer: %v", err)
	}
	_ = rw.Close()
	ro, err := OpenReadOnly(ctx, rel)
	if err != nil {
		t.Fatalf("relative path: %v", err)
	}
	defer func() { _ = ro.Close() }()
	if rows, _ := must2(ro.Messages(ctx, Filter{})); len(rows) != 1 {
		t.Fatalf("rows: %v", ids(rows))
	}
}

func TestPruneJoins(t *testing.T) {
	from := ` from messages m` + msgJoin
	for _, c := range []struct {
		where string
		keep  []string
	}{
		{` where m.deleted_at is null`, nil},
		{` where c.read_horizon_at is not null`, []string{"conversations c"}},
		{` where p.display_name like ?`, []string{"people p"}},
		{` where ` + cdnExpr + `=?`, []string{"conversations c", "conversations t"}},
		{` where t.id=?`, []string{"conversations c", "conversations t"}}, // t's own join clause reads c
		{` where abc.x=? and spc.y=?`, nil},                               // an alias inside a longer token is no reference
	} {
		got := pruneJoins(from, c.where)
		for _, j := range []string{"conversations c on", "conversations t on", "people p on"} {
			want := false
			for _, k := range c.keep {
				want = want || strings.HasPrefix(j, k)
			}
			if has := strings.Contains(got, "left join "+j); has != want {
				t.Errorf("where %q: join %q kept=%v, want %v", c.where, j, has, want)
			}
		}
	}
	// A from clause with no joins is returned as is.
	if got := pruneJoins(` from people p`, ` where p.id=?`); got != ` from people p` {
		t.Errorf("no joins: %q", got)
	}
}

// A bot's repeated post is two Teams records (own id, client id and version), and both are kept;
// the same record seen twice (it can sit in two reply chains) is one row.
func TestRepeatedBotPostsAreDistinctRecords(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	post := func(id, client string, at time.Time) teamsdesktop.Message {
		m := msg(acctA, "19:c@thread.skype", id, "Build finished", at)
		m.SenderID, m.SenderName, m.ClientMessageID = "28:bot", "Build bot", client
		return m
	}
	first, second := post("1001", "c1", base), post("1002", "c2", base.Add(2*time.Second))
	must(s.ApplyMessages(ctx, []teamsdesktop.Message{first, second}))
	again := must(s.ApplyMessages(ctx, []teamsdesktop.Message{first, first}))
	if again.Inserted != 0 || again.Unchanged != 2 {
		t.Fatalf("same record twice: %+v", again)
	}
	rows, _ := must2(s.Messages(ctx, Filter{}))
	if !eqStrings(ids(rows), []string{"1001", "1002"}) {
		t.Fatalf("both records must stay, once each: %v", ids(rows))
	}
}

func TestAbsPathFallsBackToTheGivenPath(t *testing.T) {
	old := absFn
	t.Cleanup(func() { absFn = old })
	absFn = func(string) (string, error) { return "", errors.New("no working directory") }
	if got := absPath("rel.db"); got != "rel.db" {
		t.Fatalf("absPath = %q", got)
	}
}

func TestActivityTypeListNeedsANames(t *testing.T) {
	s := newStore(t)
	for _, typ := range []string{",", " , ,"} {
		_, _, err := s.Activity(context.Background(), ActivityFilter{Type: typ})
		var coded *errs.Coded
		if !errors.As(err, &coded) || coded.Code != errs.CodeUsage {
			t.Errorf("Type %q: %v", typ, err)
		}
	}
}
