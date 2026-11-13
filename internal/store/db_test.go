package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wa.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err == nil {
		db.Close()
		t.Fatal("a database from a newer Wisp was opened")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Fatalf("unclear error: %v", err)
	}
}

func TestReopenKeepsTheSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wa.db")
	for range 2 {
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var accounts int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'accounts'`).Scan(&accounts); err != nil || accounts != 0 {
			t.Fatalf("accounts table present (%d, %v): nothing reads it", accounts, err)
		}
		db.Close()
	}
}

func TestMigrateV1ToV2KeepsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wa-v1.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const v1DDL = `
CREATE TABLE chats (
	account_id      TEXT NOT NULL,
	jid             TEXT NOT NULL,
	name            TEXT NOT NULL DEFAULT '',
	last_message_ts INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account_id, jid)
);
CREATE TABLE messages (
	account_id   TEXT NOT NULL,
	chat_jid     TEXT NOT NULL,
	id           TEXT NOT NULL,
	sender_jid   TEXT NOT NULL DEFAULT '',
	timestamp    INTEGER NOT NULL,
	message_type TEXT NOT NULL DEFAULT 'text',
	body         TEXT NOT NULL DEFAULT '',
	media_path   TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL DEFAULT '',
	from_me      INTEGER NOT NULL DEFAULT 0,
	media_meta   BLOB NOT NULL DEFAULT x'',
	wire_chat_jid   TEXT NOT NULL DEFAULT '',
	wire_sender_jid TEXT NOT NULL DEFAULT '',
	expires_at      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account_id, chat_jid, id)
);
CREATE INDEX messages_page ON messages (account_id, chat_jid, timestamp DESC, id DESC);
CREATE INDEX messages_unread ON messages (account_id, chat_jid) WHERE from_me = 0 AND status <> 'read';
CREATE INDEX messages_expiry ON messages (expires_at) WHERE expires_at > 0;
CREATE TABLE settings (
	account_id TEXT NOT NULL DEFAULT '',
	key        TEXT NOT NULL,
	value      TEXT NOT NULL,
	PRIMARY KEY (account_id, key)
);
PRAGMA user_version = 1;`
	if _, err := raw.Exec(v1DDL); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	const acct = "6281234567890"
	if _, err := raw.Exec(`INSERT INTO chats (account_id, jid, name, last_message_ts) VALUES
		(?, 'c1', 'Chat 1', 100), (?, 'c2', 'Chat 2', 200)`, acct, acct); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO messages (account_id, chat_jid, id, sender_jid, timestamp, message_type, body) VALUES
		(?, 'c1', 'm1', 's1', 100, 'text', 'hello'),
		(?, 'c1', 'm2', 's1', 101, 'text', 'world'),
		(?, 'c2', 'm3', 's2', 200, 'text', 'other')`, acct, acct, acct); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open migrated db: %v", err)
	}
	defer db.Close()

	var userVersion int
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if userVersion != schemaVersion {
		t.Fatalf("user_version = %d, want %d", userVersion, schemaVersion)
	}

	var peopleTableCount int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'people'`).Scan(&peopleTableCount); err != nil {
		t.Fatal(err)
	}
	if peopleTableCount != 1 {
		t.Fatalf("people table count = %d, want 1", peopleTableCount)
	}

	chats, err := db.ListChats(acct)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 2 {
		t.Fatalf("ListChats len = %d, want 2", len(chats))
	}

	msgsC1, err := db.GetChatMessages(acct, "c1", 0)
	if err != nil {
		t.Fatalf("GetChatMessages c1: %v", err)
	}
	if len(msgsC1) != 2 {
		t.Fatalf("GetChatMessages c1 len = %d, want 2", len(msgsC1))
	}
	for _, m := range msgsC1 {
		if m.Mentions != "" {
			t.Fatalf("migrated message Mentions = %q, want empty", m.Mentions)
		}
	}

	msgsC2, err := db.GetChatMessages(acct, "c2", 0)
	if err != nil {
		t.Fatalf("GetChatMessages c2: %v", err)
	}
	if len(msgsC2) != 1 {
		t.Fatalf("GetChatMessages c2 len = %d, want 1", len(msgsC2))
	}
	if msgsC2[0].Mentions != "" {
		t.Fatalf("migrated message Mentions = %q, want empty", msgsC2[0].Mentions)
	}
}

func TestMigrateV2ToV3KeepsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wa-v2.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schemaV1 + schemaV2 + `
		PRAGMA user_version = 2;
		INSERT INTO messages (account_id, chat_jid, id, sender_jid, timestamp, body, mentions) VALUES
			('a', 'c1', 'm1', 's1', 100, 'hello', 'x@lid'), ('a', 'c1', 'm2', 's1', 101, 'world', '');`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open v2 db: %v", err)
	}
	defer db.Close()
	var v int
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 3 {
		t.Fatalf("user_version = %d (%v), want 3", v, err)
	}
	page, err := db.GetChatMessages("a", "c1", 0)
	if err != nil || len(page) != 2 {
		t.Fatalf("GetChatMessages: len=%d, err=%v", len(page), err)
	}
	if page[1].Body != "hello" || page[1].Mentions != "x@lid" {
		t.Fatalf("migrated row changed: %+v", page[1])
	}
	for _, m := range page {
		if m.QuotedID != "" || m.QuotedSender != "" || m.QuotedBody != "" || m.QuotedType != "" {
			t.Fatalf("migrated row has quoted fields: %+v", m)
		}
	}
}
