package store

import (
	"fmt"
	"path/filepath"
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
