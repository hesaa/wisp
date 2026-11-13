// Package store is the app's own SQLite message store (whatsmeow's session store is separate).
package store

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

const schemaVersion = 3

// unread_count is deliberately not stored on chats: it is derived from messages,
// so it cannot drift from the data.
const schemaV1 = `
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
	expires_at      INTEGER NOT NULL DEFAULT 0, -- unix seconds a disappearing message goes; 0: never
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
);`

const schemaV2 = `
CREATE TABLE people (
	account_id  TEXT NOT NULL,
	jid         TEXT NOT NULL,
	pn_jid      TEXT NOT NULL DEFAULT '',
	full_name   TEXT NOT NULL DEFAULT '',
	push_name   TEXT NOT NULL DEFAULT '',
	avatar_id   TEXT NOT NULL DEFAULT '',
	avatar_ts   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account_id, jid)
);
ALTER TABLE messages ADD COLUMN mentions TEXT NOT NULL DEFAULT '';`

// Additive: rows from before v3 are not replies and keep empty strings.
const schemaV3 = `
ALTER TABLE messages ADD COLUMN quoted_id TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN quoted_sender TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN quoted_body TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN quoted_type TEXT NOT NULL DEFAULT '';`

// DB wraps the SQLite handle.
type DB struct{ sql *sql.DB }

// Open opens (creating if needed) the store at path and migrates the schema.
func Open(path string) (*DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	sqlDB, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(2) // RAM budget
	db := &DB{sql: sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return db, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

func (d *DB) migrate() error {
	var v int
	if err := d.sql.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > schemaVersion {
		return fmt.Errorf("the database was made by a newer Wisp (schema %d, this one knows %d): run the newer Wisp, or delete it to start over", v, schemaVersion)
	}
	if v == schemaVersion {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if v < 1 {
		if _, err := tx.Exec(schemaV1); err != nil {
			return err
		}
	}
	if v < 2 {
		if _, err := tx.Exec(schemaV2); err != nil {
			return err
		}
	}
	if v < 3 {
		if _, err := tx.Exec(schemaV3); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}
