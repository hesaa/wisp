package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateV3ToV4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wa-v3.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schemaV1 + schemaV2 + schemaV3 + `
		PRAGMA user_version = 3;
		INSERT INTO messages (account_id, chat_jid, id, sender_jid, timestamp, body, quoted_id) VALUES
			('a', 'c1', 'm1', 's1', 100, 'hello https://example.com', 'q0');`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open v3 db: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 4 {
		t.Fatalf("user_version = %d (%v), want 4", v, err)
	}

	var lpCount int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'link_previews'`).Scan(&lpCount); err != nil || lpCount != 1 {
		t.Fatalf("link_previews table count = %d (%v), want 1", lpCount, err)
	}

	got, ok, err := db.GetMessage("a", "c1", "m1")
	if err != nil || !ok {
		t.Fatalf("GetMessage: ok=%v, err=%v", ok, err)
	}
	if got.Body != "hello https://example.com" || got.QuotedID != "q0" {
		t.Fatalf("migrated row changed: %+v", got)
	}
	if got.LinkURL != "" || got.LinkCanonical != "" || got.LinkTitle != "" || got.LinkDesc != "" || len(got.LinkThumb) != 0 {
		t.Fatalf("migrated row has non-empty Link* fields: %+v", got)
	}
}

func TestRedeliveredUnsupportedIsReplacedNotCountedNew(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := Message{AccountID: "a", ChatJID: "c", ID: "u", Timestamp: 10, Type: "unsupported", Status: StatusRead}
	keep := Message{AccountID: "a", ChatJID: "c", ID: "t", Timestamp: 11, Type: "text", Body: "hi", Status: StatusRead}
	if err := db.InsertMessagesBatch([]Message{old, keep}); err != nil {
		t.Fatal(err)
	}
	again := old
	again.Type, again.Body, again.Status, again.MediaMeta = "video", "cap", StatusReceived, []byte{1}
	changedText := keep
	changedText.Body = "forged"
	isNew, err := db.InsertMessage(again)
	if err != nil || isNew {
		t.Fatalf("upgrade counted new=%v err=%v", isNew, err)
	}
	if _, err := db.InsertMessage(changedText); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.GetMessage("a", "c", "u")
	if err != nil || !ok || got.Type != "video" || got.Body != "cap" || got.Status != StatusRead || len(got.MediaMeta) != 1 {
		t.Fatalf("upgraded row %+v ok=%v err=%v", got, ok, err)
	}
	if got, _, _ := db.GetMessage("a", "c", "t"); got.Body != "hi" {
		t.Fatalf("a supported message was overwritten: %+v", got)
	}

	gone := Message{AccountID: "a", ChatJID: "c", ID: "g", Timestamp: 12, Type: "unsupported", Status: StatusRead}
	if err := db.InsertMessagesBatch([]Message{gone}); err != nil {
		t.Fatal(err)
	}
	tl, err := db.UnsupportedTimelines("a")
	if err != nil || len(tl) != 3 || tl[0].ID != "g" {
		t.Fatalf("timelines %+v err=%v", tl, err)
	}
	for _, id := range []string{"g", "t"} {
		if err := db.DeleteUnsupported("a", "c", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := db.GetMessage("a", "c", "g"); ok {
		t.Fatal("unsupported row not deleted")
	}
	if _, ok, _ := db.GetMessage("a", "c", "t"); !ok {
		t.Fatal("supported row deleted")
	}
}
