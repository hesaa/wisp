// Package store is the app's own SQLite message store (whatsmeow's session store is separate).
package store

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

const schemaVersion = 1

// unread_count is deliberately not stored on chats: it is derived from messages,
// so it cannot drift from the data.
const schemaV1 = `
CREATE TABLE accounts (
	account_id   TEXT PRIMARY KEY,
	display_name TEXT NOT NULL DEFAULT '',
	added_at     INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);
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
	PRIMARY KEY (account_id, chat_jid, id)
);
CREATE INDEX messages_page ON messages (account_id, chat_jid, timestamp DESC, id DESC);
CREATE TABLE settings (
	account_id TEXT NOT NULL DEFAULT '',
	key        TEXT NOT NULL,
	value      TEXT NOT NULL,
	PRIMARY KEY (account_id, key)
);`

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
	if v >= schemaVersion {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// later versions: add `if v < 2 { ... }` steps here
	if _, err := tx.Exec(schemaV1); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}
