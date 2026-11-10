package ui

import (
	"slices"
	"testing"
	"time"

	"wa-go/internal/store"
	"wa-go/internal/whatsapp"
)

const (
	acctA = "6281111111111"
	acctB = "6282222222222"
	alice = "6283333333333@s.whatsapp.net"
	bob   = "6284444444444@s.whatsapp.net"
	group = "120363000000000001@g.us"
)

func sampleChats() []store.Chat {
	return []store.Chat{
		{JID: alice, Name: "Alice", LastMessageTS: 300, Snippet: "See you tomorrow"},
		{JID: bob, Name: "Bob", LastMessageTS: 200, Snippet: "Invoice ATTACHED", Unread: 2},
		{JID: group, Name: "Family", LastMessageTS: 100, Snippet: "dinner?"},
	}
}

func jids(rows []store.Chat) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.JID)
	}
	return out
}

func TestFilterByNameOrSnippetCaseInsensitive(t *testing.T) {
	m := NewSidebarModel()
	m.SetChats(acctA, sampleChats())
	for q, want := range map[string][]string{
		"":         {alice, bob, group},
		"ALI":      {alice},
		"fam":      {group},
		"attached": {bob},   // snippet
		"Tomorrow": {alice}, // snippet, mixed case
		"zzz":      nil,
	} {
		m.SetFilter(q)
		if got := jids(m.Rows()); !slices.Equal(got, want) {
			t.Errorf("filter %q: %v, want %v", q, got, want)
		}
	}
}

func TestIncomingMessageBumpsUnreadAndMovesToTop(t *testing.T) {
	m := NewSidebarModel()
	m.SetChats(acctA, sampleChats())
	if !m.ApplyMessage(store.Message{AccountID: acctA, ChatJID: group, Timestamp: 400, Body: "on my way", Status: store.StatusReceived}) {
		t.Fatal("message for the active account not applied")
	}
	top := m.Rows()[0]
	if top.JID != group || top.Unread != 1 || top.Snippet != "on my way" || top.LastMessageTS != 400 || top.Name != "Family" {
		t.Fatalf("top row %+v", top)
	}
	if m.Unread() != 3 {
		t.Fatalf("total unread %d, want 3", m.Unread())
	}
	// a chat not listed yet appears on top
	m.ApplyMessage(store.Message{AccountID: acctA, ChatJID: "6285555555555@s.whatsapp.net", Timestamp: 500, Body: "hi", Status: store.StatusReceived})
	if top := m.Rows()[0]; top.JID != "6285555555555@s.whatsapp.net" || top.Unread != 1 || len(m.Rows()) != 4 {
		t.Fatalf("new chat row %+v (rows %d)", top, len(m.Rows()))
	}
	m.MarkRead(bob)
	if m.Unread() != 2 {
		t.Fatalf("total unread %d after reading Bob, want 2", m.Unread())
	}
}

func TestFromMeDoesNotBumpUnread(t *testing.T) {
	m := NewSidebarModel()
	m.SetChats(acctA, sampleChats())
	m.ApplyMessage(store.Message{AccountID: acctA, ChatJID: group, Timestamp: 400, Body: "ok", FromMe: true, Status: store.StatusSent})
	if top := m.Rows()[0]; top.JID != group || top.Unread != 0 || top.Snippet != "ok" {
		t.Fatalf("top row %+v", top)
	}
}

func TestOtherAccountMessageLeavesListAlone(t *testing.T) {
	m := NewSidebarModel()
	m.SetChats(acctA, sampleChats())
	if m.ApplyMessage(store.Message{AccountID: acctB, ChatJID: group, Timestamp: 400, Body: "x", Status: store.StatusReceived}) {
		t.Fatal("message for another account applied")
	}
	if !slices.Equal(m.Rows(), sampleChats()) {
		t.Fatalf("rows changed: %+v", m.Rows())
	}
}

func TestStatusBroadcastNeverARow(t *testing.T) {
	m := NewSidebarModel()
	m.SetChats(acctA, append(sampleChats(), store.Chat{JID: statusBroadcast, LastMessageTS: 900}))
	if m.ApplyMessage(store.Message{AccountID: acctA, ChatJID: statusBroadcast, Timestamp: 999, Body: "story", Status: store.StatusReceived}) {
		t.Fatal("story applied as a chat")
	}
	if slices.Contains(jids(m.Rows()), statusBroadcast) || m.Unread() != 2 {
		t.Fatalf("rows %v unread %d", jids(m.Rows()), m.Unread())
	}
}

func TestAccountSwitchFiresActiveChanged(t *testing.T) {
	m := NewRailModel()
	var got []string
	m.OnActiveChanged(func(id string) { got = append(got, id) })
	m.SetAccounts([]string{acctA, acctB})
	if m.Active() != acctA || !slices.Equal(got, []string{acctA}) {
		t.Fatalf("after SetAccounts active=%q events=%v", m.Active(), got)
	}
	m.SetActive(acctB)
	if m.Active() != acctB || !slices.Equal(got, []string{acctA, acctB}) {
		t.Fatalf("after switch active=%q events=%v", m.Active(), got)
	}
	m.SetActive(acctB)     // same account
	m.SetActive("unknown") // not an account
	if m.Active() != acctB || len(got) != 2 {
		t.Fatalf("no-op switches fired: active=%q events=%v", m.Active(), got)
	}
	m.SetAccounts([]string{acctA}) // active account removed: falls back to the first
	if m.Active() != acctA || len(got) != 3 {
		t.Fatalf("after removal active=%q events=%v", m.Active(), got)
	}
}

func TestPhoneJID(t *testing.T) {
	for in, want := range map[string]string{
		"+62 812-3456-7890": "6281234567890@s.whatsapp.net",
		"(021) 555 0123":    "0215550123@s.whatsapp.net",
		"1234567":           "1234567@s.whatsapp.net",
		"123456789012345":   "123456789012345@s.whatsapp.net",
	} {
		if got, ok := phoneJID(in); !ok || got != want {
			t.Errorf("phoneJID(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "123456", "1234567890123456", "Alice", "62812x4567", "+", "62.812.3456"} {
		if got, ok := phoneJID(in); ok {
			t.Errorf("phoneJID(%q) = %q, want invalid", in, got)
		}
	}
}

func TestNewChatRows(t *testing.T) {
	cands := newChatCandidates(
		[]store.Chat{{JID: alice, Name: "Alice"}, {JID: group, Name: "Family"}},
		[]whatsapp.Contact{{JID: alice, Name: "Alice Smith"}, {JID: bob, Name: "Bob"}},
	)
	if got := jids(cands); !slices.Equal(got, []string{alice, group, bob}) {
		t.Fatalf("candidates %v (chats first, contacts deduped)", got)
	}
	if got := jids(newChatRows(cands, "BO")); !slices.Equal(got, []string{bob}) {
		t.Fatalf("by name: %v", got)
	}
	if got := jids(newChatRows(cands, "833333")); !slices.Equal(got, []string{alice}) {
		t.Fatalf("by number: %v", got)
	}
	if got := jids(newChatRows(cands, "+62 844")); !slices.Equal(got, []string{bob}) {
		t.Fatalf("by formatted partial number: %v", got)
	}
	if got := jids(newChatRows(cands, "+1 (555) 010-0999")); !slices.Equal(got, []string{"15550100999@s.whatsapp.net"}) {
		t.Fatalf("typed number: %v", got)
	}
	if got := jids(newChatRows(cands, "+62 833 3333 3333")); !slices.Equal(got, []string{alice}) {
		t.Fatalf("typed number of a known chat must not add a second row: %v", got)
	}
}

func TestRelTime(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.Local) // a Tuesday
	at := func(d, h int) int64 { return time.Date(2026, 11, d, h, 4, 0, 0, time.Local).Unix() }
	for ts, want := range map[int64]string{
		0:         "",
		at(10, 9): "09:04",
		at(9, 23): "Yesterday",
		at(4, 9):  "Wednesday",
		at(3, 9):  "03/11/2026",
	} {
		if got := relTime(ts, now); got != want {
			t.Errorf("relTime(%d) = %q, want %q", ts, got, want)
		}
	}
}

func TestInitialsAndDisplayName(t *testing.T) {
	for _, c := range []struct{ name, jid, want string }{
		{"Alice Smith", alice, "AS"},
		{"mom ❤", alice, "M"},
		{"", alice, "33"},
		{"❤❤", bob, "44"},
	} {
		if got := initials(c.name, c.jid); got != c.want {
			t.Errorf("initials(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	if got := displayName(store.Chat{JID: alice}); got != "+6283333333333" {
		t.Errorf("displayName fallback %q", got)
	}
}
