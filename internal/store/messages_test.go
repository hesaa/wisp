package store

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const acct = "6281234567890"

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "wa.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func msgs(account, chat string, n int, fromMe bool) []Message {
	out := make([]Message, n)
	for i := range out {
		out[i] = Message{ID: fmt.Sprintf("m%d", i), ChatJID: chat, AccountID: account, SenderJID: "s@x",
			Timestamp: int64(1000 + i), Type: "text", Body: fmt.Sprintf("body %d", i), FromMe: fromMe, Status: StatusReceived}
	}
	return out
}

func TestPagination(t *testing.T) {
	db := openTest(t)
	if err := db.InsertMessagesBatch(msgs(acct, "c1", 120, false)); err != nil {
		t.Fatal(err)
	}
	p0, err := db.GetChatMessages(acct, "c1", 0)
	if err != nil || len(p0) != 50 {
		t.Fatalf("page0 len=%d err=%v", len(p0), err)
	}
	if p0[0].ID != "m119" || p0[49].ID != "m70" {
		t.Fatalf("page0 order: %s..%s", p0[0].ID, p0[49].ID)
	}
	p1, _ := db.GetChatMessages(acct, "c1", 1)
	if len(p1) != 50 || p1[0].ID != "m69" {
		t.Fatalf("page1 len=%d first=%s", len(p1), p1[0].ID)
	}
	p2, _ := db.GetChatMessages(acct, "c1", 2)
	if len(p2) != 20 || p2[19].ID != "m0" {
		t.Fatalf("page2 len=%d", len(p2))
	}
	p3, err := db.GetChatMessages(acct, "c1", 3)
	if err != nil || len(p3) != 0 {
		t.Fatalf("page3 len=%d err=%v", len(p3), err)
	}
}

func TestPaginationTiedTimestamps(t *testing.T) {
	db := openTest(t)
	ms := msgs(acct, "c1", 120, false)
	for i := range ms {
		ms[i].Timestamp = 5
	}
	db.InsertMessagesBatch(ms)
	seen := map[string]bool{}
	for p := 0; p < 3; p++ {
		page, _ := db.GetChatMessages(acct, "c1", p)
		for _, m := range page {
			if seen[m.ID] {
				t.Fatalf("duplicate %s across pages", m.ID)
			}
			seen[m.ID] = true
		}
	}
	if len(seen) != 120 {
		t.Fatalf("saw %d", len(seen))
	}
}

func TestBatchChunkingAndIdempotent(t *testing.T) {
	db := openTest(t)
	ms := msgs(acct, "c1", 250, false)
	if err := db.InsertMessagesBatch(ms); err != nil {
		t.Fatal(err)
	}
	var n int
	db.sql.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	if n != 250 {
		t.Fatalf("stored %d", n)
	}
	if err := db.MarkChatRead(acct, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertMessagesBatch(ms[:10]); err != nil {
		t.Fatal(err)
	}
	db.sql.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	chats, _ := db.ListChats(acct)
	if n != 250 || chats[0].Unread != 0 {
		t.Fatalf("after redelivery count=%d unread=%d", n, chats[0].Unread)
	}
}

func TestUnreadAndStatus(t *testing.T) {
	db := openTest(t)
	in := msgs(acct, "c1", 5, false)
	out := msgs(acct, "c1", 3, true)
	for i := range out {
		out[i].ID = fmt.Sprintf("o%d", i)
		out[i].Status = StatusSent
	}
	db.InsertMessagesBatch(append(in, out...))
	chats, _ := db.ListChats(acct)
	if chats[0].Unread != 5 {
		t.Fatalf("unread=%d want 5 (outgoing excluded)", chats[0].Unread)
	}
	if err := db.UpdateMessageStatus(acct, "c1", []string{"m0", "m1"}, StatusRead); err != nil {
		t.Fatal(err)
	}
	chats, _ = db.ListChats(acct)
	if chats[0].Unread != 3 {
		t.Fatalf("unread=%d want 3", chats[0].Unread)
	}
	db.MarkChatRead(acct, "c1")
	chats, _ = db.ListChats(acct)
	if chats[0].Unread != 0 {
		t.Fatalf("unread=%d want 0", chats[0].Unread)
	}
	page, _ := db.GetChatMessages(acct, "c1", 0)
	for _, m := range page {
		if m.FromMe && m.Status != StatusSent {
			t.Fatalf("MarkChatRead touched outgoing status %s", m.Status)
		}
	}
}

func TestAccountIsolation(t *testing.T) {
	db := openTest(t)
	db.InsertMessagesBatch(msgs("A", "g@g.us", 7, false))
	db.InsertMessagesBatch(msgs("B", "g@g.us", 3, false))
	pa, _ := db.GetChatMessages("A", "g@g.us", 0)
	pb, _ := db.GetChatMessages("B", "g@g.us", 0)
	if len(pa) != 7 || len(pb) != 3 {
		t.Fatalf("A=%d B=%d", len(pa), len(pb))
	}
	ca, _ := db.ListChats("A")
	cb, _ := db.ListChats("B")
	if ca[0].Unread != 7 || cb[0].Unread != 3 {
		t.Fatalf("unread A=%d B=%d", ca[0].Unread, cb[0].Unread)
	}
	db.MarkChatRead("A", "g@g.us")
	cb, _ = db.ListChats("B")
	if cb[0].Unread != 3 {
		t.Fatal("marking A read affected B")
	}
}

func TestChatListOrderAndSnippet(t *testing.T) {
	db := openTest(t)
	db.UpsertChat(acct, "old", "Old Chat", 10)
	db.UpsertChat(acct, "new", "New Chat", 20)
	db.InsertMessagesBatch([]Message{{ID: "1", ChatJID: "old", AccountID: acct, Timestamp: 10, Body: "hello", Status: StatusReceived}})
	chats, _ := db.ListChats(acct)
	if len(chats) != 2 || chats[0].JID != "new" || chats[1].Name != "Old Chat" || chats[1].Snippet != "hello" {
		t.Fatalf("%+v", chats)
	}
	// message on old chat makes it newest; name survives
	db.InsertMessagesBatch([]Message{{ID: "2", ChatJID: "old", AccountID: acct, Timestamp: 30, Body: "latest", Status: StatusReceived}})
	chats, _ = db.ListChats(acct)
	if chats[0].JID != "old" || chats[0].Name != "Old Chat" || chats[0].Snippet != "latest" || chats[0].LastMessageTS != 30 {
		t.Fatalf("%+v", chats[0])
	}
}

func TestStatusNeverRegresses(t *testing.T) {
	db := openTest(t)
	db.InsertMessagesBatch(msgs(acct, "c1", 1, false))
	if err := db.UpdateMessageStatus(acct, "c1", []string{"m0"}, StatusRead); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMessageStatus(acct, "c1", []string{"m0"}, StatusDelivered); err != nil {
		t.Fatal(err)
	}
	page, _ := db.GetChatMessages(acct, "c1", 0)
	chats, _ := db.ListChats(acct)
	if page[0].Status != StatusRead || chats[0].Unread != 0 {
		t.Fatalf("status=%s unread=%d", page[0].Status, chats[0].Unread)
	}
	if err := db.UpdateMessageStatus(acct, "c1", []string{"m0"}, "bogus"); err == nil {
		t.Fatal("unknown status accepted")
	}
}

func TestRecentStories(t *testing.T) {
	db := openTest(t)
	story := func(account, id, sender string, ts int64, fromMe bool) Message {
		return Message{ID: id, ChatJID: statusBroadcastJID, AccountID: account, SenderJID: sender, Timestamp: ts, FromMe: fromMe, Status: StatusReceived}
	}
	if err := db.InsertMessagesBatch([]Message{
		story(acct, "1", "alice", 100, false),
		story(acct, "2", "alice", 300, false),
		story(acct, "3", "bob", 200, false),
		story(acct, "4", "carol", 50, false), // older than since
		story(acct, "5", "me", 400, true),    // own status
		story("other", "6", "dave", 300, false),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertChat(acct, "alice", "Alice", 0); err != nil {
		t.Fatal(err)
	}
	got, err := db.RecentStories(acct, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []Story{{SenderJID: "alice", Name: "Alice", LastTS: 300}, {SenderJID: "bob", LastTS: 200}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestStatusMessages(t *testing.T) {
	db := openTest(t)
	story := func(account, id, sender string, ts int64) Message {
		return Message{ID: id, ChatJID: statusBroadcastJID, AccountID: account, SenderJID: sender, Timestamp: ts, Status: StatusReceived}
	}
	in := []Message{
		story(acct, "b", "alice", 200), story(acct, "a", "alice", 200), // same second: ordered by id
		story(acct, "c", "alice", 300), story(acct, "old", "alice", 99), // just before since
		story(acct, "x", "bob", 250), story("other", "y", "alice", 250),
		{ID: "chat", ChatJID: "alice", AccountID: acct, SenderJID: "alice", Timestamp: 250, Status: StatusReceived}, // not a status
	}
	if err := db.InsertMessagesBatch(in); err != nil {
		t.Fatal(err)
	}
	ids := func(ms []Message) string {
		s := ""
		for _, m := range ms {
			s += m.ID + ","
		}
		return s
	}
	got, err := db.StatusMessages(acct, "alice", 100)
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != "a,b,c," {
		t.Fatalf("got %s, want a,b,c, (oldest first, own account and sender only, since is inclusive)", ids(got))
	}
	if got, _ = db.StatusMessages(acct, "alice", 99); ids(got) != "old,a,b,c," {
		t.Fatalf("since 99: got %s", ids(got))
	}
	if got, _ = db.StatusMessages(acct, "alice", 301); len(got) != 0 {
		t.Fatalf("since 301: got %s", ids(got))
	}
	var many []Message
	for i := range maxStatusMessages + 5 {
		many = append(many, story(acct, fmt.Sprintf("m%03d", i), "carol", int64(1000+i)))
	}
	if err := db.InsertMessagesBatch(many); err != nil {
		t.Fatal(err)
	}
	got, err = db.StatusMessages(acct, "carol", 0)
	if err != nil || len(got) != maxStatusMessages || got[0].ID != "m005" || got[len(got)-1].ID != fmt.Sprintf("m%03d", maxStatusMessages+4) {
		t.Fatalf("cap: %d messages from %s (err %v), want the newest %d, oldest first", len(got), got[0].ID, err, maxStatusMessages)
	}
}

func TestMediaMetaRoundTrip(t *testing.T) {
	db := openTest(t)
	ms := msgs(acct, "c1", 2, false)
	ms[1].MediaMeta = []byte{1, 2, 3}
	if err := db.InsertMessagesBatch(ms); err != nil {
		t.Fatal(err)
	}
	page, _ := db.GetChatMessages(acct, "c1", 0)
	if string(page[0].MediaMeta) != "\x01\x02\x03" || len(page[1].MediaMeta) != 0 {
		t.Fatalf("meta %v / %v", page[0].MediaMeta, page[1].MediaMeta)
	}
}

func TestGetMessageIsAccountScoped(t *testing.T) {
	db := openTest(t)
	mine, other := msgs(acct, "c1", 1, false)[0], msgs("6289999999999", "c1", 1, true)[0]
	other.Body, other.Status = "other account", StatusRead
	mine.WireSenderJID = "123@lid"
	if err := db.InsertMessagesBatch([]Message{mine, other}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.GetMessage(acct, "c1", "m0")
	if err != nil || !ok || got.Body != "body 0" || got.FromMe || got.Status != StatusReceived || got.WireSenderJID != "123@lid" {
		t.Fatalf("GetMessage = %+v, %v, %v", got, ok, err)
	}
	if got, ok, _ := db.GetMessage("6289999999999", "c1", "m0"); !ok || got.Body != "other account" {
		t.Fatalf("other account's copy = %+v, %v", got, ok)
	}
	for _, k := range [][3]string{{acct, "c2", "m0"}, {acct, "c1", "m1"}, {"6280000000000", "c1", "m0"}} {
		if got, ok, err := db.GetMessage(k[0], k[1], k[2]); ok || err != nil || got.ID != "" {
			t.Fatalf("GetMessage%v = %+v, %v, %v; want not found", k, got, ok, err)
		}
	}
}

func TestMarkChatReadUntilLeavesLaterMessagesUnread(t *testing.T) {
	db := openTest(t)
	db.InsertMessagesBatch(msgs(acct, "c1", 3, false)) // timestamps 1000..1002
	db.InsertMessagesBatch(msgs("other", "c1", 3, false))
	if err := db.MarkChatReadUntil(acct, "c1", 1001); err != nil {
		t.Fatal(err)
	}
	unread := func(account string) int {
		chats, err := db.ListChats(account)
		if err != nil || len(chats) != 1 {
			t.Fatalf("ListChats(%s) = %v, %v", account, chats, err)
		}
		return chats[0].Unread
	}
	if got := unread(acct); got != 1 {
		t.Fatalf("unread after read until 1001 = %d, want 1 (the later message)", got)
	}
	if got := unread("other"); got != 3 {
		t.Fatalf("another account's unread = %d, want 3", got)
	}
}

func TestClearMediaPathsForgetsEveryDownload(t *testing.T) {
	db := openTest(t)
	ms := msgs(acct, "c1", 2, false)
	ms[0].MediaPath = `C:\cache\a.jpg`
	db.InsertMessagesBatch(ms)
	if err := db.ClearMediaPaths(); err != nil {
		t.Fatal(err)
	}
	page, _ := db.GetChatMessages(acct, "c1", 0)
	for _, m := range page {
		if m.MediaPath != "" {
			t.Fatalf("%s still points at %q", m.ID, m.MediaPath)
		}
	}
}

func TestPurgeExpiredDeletesOnlyDisappearedMessages(t *testing.T) {
	db := openTest(t)
	ms := msgs(acct, "c1", 3, false)
	ms[0].ExpiresAt, ms[0].MediaPath = 1500, `C:\cache\gone.jpg` // expired
	ms[1].ExpiresAt = 3000                                       // not yet
	db.InsertMessagesBatch(ms)
	paths, n, err := db.PurgeExpired(2000)
	if err != nil || n != 1 || len(paths) != 1 || paths[0] != `C:\cache\gone.jpg` {
		t.Fatalf("PurgeExpired = %v, %d, %v", paths, n, err)
	}
	page, _ := db.GetChatMessages(acct, "c1", 0)
	if len(page) != 2 || page[1].ID != "m1" || page[1].ExpiresAt != 3000 {
		t.Fatalf("left: %+v", page)
	}
	if exp, err := db.ChatExpiration(acct, "c1"); err != nil || exp != 0 {
		t.Fatalf("ChatExpiration with a lasting newest message = %d, %v", exp, err)
	}
}

func TestChatExpirationFollowsTheNewestMessage(t *testing.T) {
	db := openTest(t)
	ms := msgs(acct, "c1", 2, false)
	ms[1].ExpiresAt = ms[1].Timestamp + 86400
	db.InsertMessagesBatch(ms)
	if exp, err := db.ChatExpiration(acct, "c1"); err != nil || exp != 86400 {
		t.Fatalf("ChatExpiration = %d, %v; want 86400", exp, err)
	}
	if exp, err := db.ChatExpiration("other", "c1"); err != nil || exp != 0 {
		t.Fatalf("another account's ChatExpiration = %d, %v", exp, err)
	}
}

func TestChatName(t *testing.T) {
	db := openTest(t)
	db.UpsertChat(acct, "g@g.us", "Team", 0)
	if n, err := db.ChatName(acct, "g@g.us"); err != nil || n != "Team" {
		t.Fatalf("ChatName = %q, %v", n, err)
	}
	if n, err := db.ChatName("other", "g@g.us"); err != nil || n != "" {
		t.Fatalf("another account's ChatName = %q, %v", n, err)
	}
}

func TestUnreadCountUsesThePartialIndex(t *testing.T) {
	db := openTest(t)
	rows, err := db.sql.Query(`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM messages m WHERE m.account_id = ? AND m.chat_jid = ?
		AND m.from_me = 0 AND m.status <> 'read'`, acct, "c1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if !strings.Contains(plan, "messages_unread") {
		t.Fatalf("unread count does not use messages_unread:\n%s", plan)
	}
}

func TestMentionsRoundTrip(t *testing.T) {
	db := openTest(t)
	const acct = "6281234567890"

	m := Message{
		ID:        "m1",
		ChatJID:   "c1",
		AccountID: acct,
		SenderJID: "s@s.whatsapp.net",
		Timestamp: 1000,
		Type:      "text",
		Body:      "hello @a @b",
		Mentions:  "a@lid b@s.whatsapp.net",
		Status:    StatusReceived,
	}

	newMsg, err := db.InsertMessage(m)
	if err != nil || !newMsg {
		t.Fatalf("InsertMessage: new=%v, err=%v", newMsg, err)
	}

	readM, ok, err := db.GetMessage(acct, "c1", "m1")
	if err != nil || !ok {
		t.Fatalf("GetMessage: ok=%v, err=%v", ok, err)
	}
	if readM.Mentions != "a@lid b@s.whatsapp.net" {
		t.Fatalf("GetMessage Mentions = %q, want 'a@lid b@s.whatsapp.net'", readM.Mentions)
	}

	page, err := db.GetChatMessages(acct, "c1", 0)
	if err != nil || len(page) != 1 {
		t.Fatalf("GetChatMessages: len=%d, err=%v", len(page), err)
	}
	if page[0].Mentions != "a@lid b@s.whatsapp.net" {
		t.Fatalf("GetChatMessages Mentions = %q, want 'a@lid b@s.whatsapp.net'", page[0].Mentions)
	}
}

func TestListChatsLastMessageFields(t *testing.T) {
	db := openTest(t)
	const acct = "6281234567890"

	meta := []byte{0x01, 0x02, 0x03, 0x04}
	// Older message: text from other sender
	m1 := Message{
		ID:        "m1",
		ChatJID:   "c1",
		AccountID: acct,
		SenderJID: "other@s.whatsapp.net",
		Timestamp: 100,
		Type:      "text",
		Body:      "old text",
		FromMe:    false,
		Status:    StatusReceived,
	}
	// Newest message: own image without body
	m2 := Message{
		ID:        "m2",
		ChatJID:   "c1",
		AccountID: acct,
		SenderJID: acct,
		Timestamp: 200,
		Type:      "image",
		Body:      "",
		FromMe:    true,
		MediaMeta: meta,
		Status:    StatusSent,
	}

	if err := db.InsertMessagesBatch([]Message{m1, m2}); err != nil {
		t.Fatalf("InsertMessagesBatch: %v", err)
	}

	chats, err := db.ListChats(acct)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 1 {
		t.Fatalf("ListChats len = %d, want 1", len(chats))
	}
	c := chats[0]
	if c.Snippet != "" {
		t.Fatalf("Snippet = %q, want ''", c.Snippet)
	}
	if c.LastType != "image" {
		t.Fatalf("LastType = %q, want 'image'", c.LastType)
	}
	if c.LastSender != acct {
		t.Fatalf("LastSender = %q, want %q", c.LastSender, acct)
	}
	if !c.LastFromMe {
		t.Fatalf("LastFromMe = false, want true")
	}
	if !bytes.Equal(c.LastMeta, meta) {
		t.Fatalf("LastMeta = %v, want %v", c.LastMeta, meta)
	}

	// Chat with no messages
	if err := db.UpsertChat(acct, "c2", "Empty Chat", 0); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	chats, err = db.ListChats(acct)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 2 {
		t.Fatalf("ListChats len = %d, want 2", len(chats))
	}
	// chats are ordered by last_message_ts DESC, so c1 (ts 200) is first, c2 (ts 0) is second
	emptyChat := chats[1]
	if emptyChat.JID != "c2" {
		emptyChat = chats[0]
	}
	if emptyChat.Snippet != "" || emptyChat.LastType != "" || emptyChat.LastSender != "" || emptyChat.LastFromMe != false || len(emptyChat.LastMeta) != 0 {
		t.Fatalf("emptyChat unexpected: %+v", emptyChat)
	}
}

func TestQuotedMessageRoundTrip(t *testing.T) {
	db := openTest(t)
	m := Message{ID: "m1", ChatJID: "c1", AccountID: "a", Timestamp: 1000, Type: "text", Body: "reply",
		QuotedID: "msg123", QuotedSender: "alice@s.whatsapp.net", QuotedBody: "hello", QuotedType: "text"}
	if isNew, err := db.InsertMessage(m); err != nil || !isNew {
		t.Fatalf("InsertMessage: new=%v, err=%v", isNew, err)
	}
	got, ok, err := db.GetMessage("a", "c1", "m1")
	if err != nil || !ok {
		t.Fatalf("GetMessage: ok=%v, err=%v", ok, err)
	}
	page, err := db.GetChatMessages("a", "c1", 0)
	if err != nil || len(page) != 1 {
		t.Fatalf("GetChatMessages: len=%d, err=%v", len(page), err)
	}
	for _, r := range []Message{got, page[0]} {
		if r.QuotedID != m.QuotedID || r.QuotedSender != m.QuotedSender || r.QuotedBody != m.QuotedBody || r.QuotedType != m.QuotedType {
			t.Fatalf("quoted fields = %+v, want %+v", r, m)
		}
	}
}
