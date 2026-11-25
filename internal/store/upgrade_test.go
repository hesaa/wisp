package store

import (
	"path/filepath"
	"testing"
)

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
