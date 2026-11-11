package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
)

const (
	// PageSize is the number of messages per page.
	PageSize = 50
	// BatchSize is the number of rows committed per transaction on bulk insert.
	BatchSize = 100

	StatusReceived  = "received" // incoming, not yet read locally
	StatusSent      = "sent"
	StatusDelivered = "delivered"
	StatusRead      = "read"

	// statusBroadcastJID holds WhatsApp stories: stored and read by chat JID, never listed as a chat.
	statusBroadcastJID = "status@broadcast"
)

// Message is one stored message. Incoming messages are unread until Status is StatusRead.
type Message struct {
	ID        string
	ChatJID   string
	AccountID string
	SenderJID string
	Timestamp int64 // unix seconds
	Type      string
	Body      string
	MediaPath string
	Status    string
	FromMe    bool
	MediaMeta []byte // marshaled media sub-message (keys + embedded thumbnail) for on-demand download
	// Chat and sender exactly as received (an LID or a phone number), for addressing read receipts
	// back the way the server expects; empty when unknown (history, own messages).
	WireChatJID, WireSenderJID string
	ExpiresAt                  int64  // unix seconds a disappearing message goes (PurgeExpired); 0: never
	Mentions                   string // space-separated JIDs
}

// Chat is a sidebar row.
type Chat struct {
	JID           string
	Name          string
	LastMessageTS int64
	Snippet       string
	Unread        int
	LastType      string
	LastSender    string
	LastFromMe    bool
	LastMeta      []byte
}

// InsertMessagesBatch stores msgs in transactions of BatchSize rows. Messages already
// stored (same account, chat, id) are ignored. Chats are created/bumped as needed
// (never for status@broadcast).
func (d *DB) InsertMessagesBatch(msgs []Message) error {
	for start := 0; start < len(msgs); start += BatchSize {
		end := min(start+BatchSize, len(msgs))
		if _, err := d.insertChunk(msgs[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// InsertMessage stores one message like InsertMessagesBatch and reports whether it was new
// (false for a redelivered duplicate).
func (d *DB) InsertMessage(m Message) (bool, error) {
	n, err := d.insertChunk([]Message{m})
	return n == 1, err
}

// insertChunk inserts msgs in one transaction and returns how many were new.
func (d *DB) insertChunk(msgs []Message) (int64, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(`INSERT OR IGNORE INTO messages
		(account_id, chat_jid, id, sender_jid, timestamp, message_type, body, media_path, status, from_me, media_meta,
		 wire_chat_jid, wire_sender_jid, expires_at, mentions)
		VALUES (?,?,?,?,?,?,?,?,?,?,COALESCE(?, x''),?,?,?,?)`) // COALESCE: a nil []byte binds as NULL
	if err != nil {
		return 0, err
	}
	defer ins.Close()
	chat, err := tx.Prepare(`INSERT INTO chats (account_id, jid, last_message_ts) VALUES (?,?,?)
		ON CONFLICT(account_id, jid) DO UPDATE SET last_message_ts = MAX(last_message_ts, excluded.last_message_ts)`)
	if err != nil {
		return 0, err
	}
	defer chat.Close()
	var inserted int64
	for _, m := range msgs {
		res, err := ins.Exec(m.AccountID, m.ChatJID, m.ID, m.SenderJID, m.Timestamp, m.Type, m.Body, m.MediaPath, m.Status, m.FromMe, m.MediaMeta,
			m.WireChatJID, m.WireSenderJID, m.ExpiresAt, m.Mentions)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		inserted += n
		if m.ChatJID == statusBroadcastJID {
			continue
		}
		if _, err := chat.Exec(m.AccountID, m.ChatJID, m.Timestamp); err != nil {
			return 0, err
		}
	}
	return inserted, tx.Commit()
}

// GetChatMessages returns one page (0-based) of a chat, newest first. A page past the end is empty.
func (d *DB) GetChatMessages(accountID, chatJID string, page int) ([]Message, error) {
	if page < 0 {
		page = 0
	}
	rows, err := d.sql.Query(`SELECT `+messageCols+`
		FROM messages WHERE account_id = ? AND chat_jid = ?
		ORDER BY timestamp DESC, id DESC LIMIT ? OFFSET ?`, accountID, chatJID, PageSize, page*PageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// GetMessage returns one stored message of accountID; ok is false when it is not stored.
func (d *DB) GetMessage(accountID, chatJID, id string) (m Message, ok bool, err error) {
	rows, err := d.sql.Query(`SELECT `+messageCols+` FROM messages WHERE account_id = ? AND chat_jid = ? AND id = ?`,
		accountID, chatJID, id)
	if err != nil {
		return m, false, err
	}
	defer rows.Close()
	ms, err := scanMessages(rows)
	if err != nil || len(ms) == 0 {
		return m, false, err
	}
	return ms[0], true, nil
}

const messageCols = `id, chat_jid, account_id, sender_jid, timestamp, message_type, body, media_path, status, from_me, media_meta,
		wire_chat_jid, wire_sender_jid, expires_at, mentions`

func scanMessages(rows *sql.Rows) ([]Message, error) {
	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.AccountID, &m.SenderJID, &m.Timestamp, &m.Type, &m.Body, &m.MediaPath, &m.Status, &m.FromMe, &m.MediaMeta,
			&m.WireChatJID, &m.WireSenderJID, &m.ExpiresAt, &m.Mentions); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

var statusRank = map[string]int{StatusReceived: 0, StatusSent: 1, StatusDelivered: 2, StatusRead: 3}

// rankSQL must mirror statusRank; stored statuses outside it (e.g. ”) rank lowest.
const rankSQL = `CASE status WHEN 'received' THEN 0 WHEN 'sent' THEN 1 WHEN 'delivered' THEN 2 WHEN 'read' THEN 3 ELSE -1 END`

// UpdateMessageStatus raises the status of specific messages (delivery/read receipts).
// Status never moves backwards (a late "delivered" cannot undo "read"); an unknown status is an error.
func (d *DB) UpdateMessageStatus(accountID, chatJID string, ids []string, status string) error {
	rank, ok := statusRank[status]
	if !ok {
		return fmt.Errorf("store: unknown message status %q", status)
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`UPDATE messages SET status = ? WHERE account_id = ? AND chat_jid = ? AND id = ? AND (` + rankSQL + `) < ?`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, id := range ids {
		if _, err := st.Exec(status, accountID, chatJID, id, rank); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetMediaPath records where a message's downloaded media was cached.
func (d *DB) SetMediaPath(accountID, chatJID, id, path string) error {
	_, err := d.sql.Exec(`UPDATE messages SET media_path = ? WHERE account_id = ? AND chat_jid = ? AND id = ?`, path, accountID, chatJID, id)
	return err
}

// MessageSender returns the sender JID of a stored message, or "" when it is not stored.
func (d *DB) MessageSender(accountID, chatJID, id string) (string, error) {
	var sender string
	err := d.sql.QueryRow(`SELECT sender_jid FROM messages WHERE account_id = ? AND chat_jid = ? AND id = ?`,
		accountID, chatJID, id).Scan(&sender)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return sender, err
}

// ReviseMessage rewrites a stored message's body after an edit. A non-empty msgType (a
// delete-for-everyone) also replaces its type and unlinks its media (keys and cached path; the
// cached file stays on disk). It reports whether the row changed.
func (d *DB) ReviseMessage(accountID, chatJID, id, msgType, body string) (bool, error) {
	res, err := d.sql.Exec(`UPDATE messages SET body = ?4,
		message_type = COALESCE(NULLIF(?5, ''), message_type),
		media_meta = CASE WHEN ?5 = '' THEN media_meta ELSE x'' END,
		media_path = CASE WHEN ?5 = '' THEN media_path ELSE '' END
		WHERE account_id = ?1 AND chat_jid = ?2 AND id = ?3
		AND (body <> ?4 OR message_type <> COALESCE(NULLIF(?5, ''), message_type))`,
		accountID, chatJID, id, body, msgType)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkChatRead marks all incoming messages in the chat as read.
func (d *DB) MarkChatRead(accountID, chatJID string) error {
	return d.MarkChatReadUntil(accountID, chatJID, math.MaxInt64)
}

// MarkChatReadUntil marks the chat's incoming messages up to until (unix seconds) as read: the chat
// was read on another device then, and later messages stay unread.
func (d *DB) MarkChatReadUntil(accountID, chatJID string, until int64) error {
	_, err := d.sql.Exec(`UPDATE messages SET status = 'read' WHERE account_id = ? AND chat_jid = ? AND from_me = 0
		AND status <> 'read' AND timestamp <= ?`, accountID, chatJID, until)
	return err
}

// ClearMediaPaths forgets every download (after the media cache was cleared), so the media shows as
// downloadable again.
func (d *DB) ClearMediaPaths() error {
	_, err := d.sql.Exec(`UPDATE messages SET media_path = '' WHERE media_path <> ''`)
	return err
}

// PurgeExpired deletes every disappearing message whose time ran out by now (unix seconds), of every
// account. It returns the cached media files they pointed at, for the caller to delete, and how many
// messages went.
func (d *DB) PurgeExpired(now int64) (paths []string, n int64, err error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT media_path FROM messages WHERE expires_at > 0 AND expires_at <= ? AND media_path <> ''`, now)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, 0, err
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	res, err := tx.Exec(`DELETE FROM messages WHERE expires_at > 0 AND expires_at <= ?`, now)
	if err != nil {
		return nil, 0, err
	}
	if n, err = res.RowsAffected(); err != nil {
		return nil, 0, err
	}
	return paths, n, tx.Commit()
}

// ChatExpiration returns the disappearing-messages timer of a chat in seconds (0: off), as its newest
// message carries it.
// shortcut: a timer change shows from the next message on; store the chat's timer from
// EPHEMERAL_SETTING / group info if sends right after a change must follow it.
func (d *DB) ChatExpiration(accountID, chatJID string) (int64, error) {
	var exp int64
	err := d.sql.QueryRow(`SELECT CASE WHEN expires_at > 0 THEN expires_at - timestamp ELSE 0 END FROM messages
		WHERE account_id = ? AND chat_jid = ? ORDER BY timestamp DESC, id DESC LIMIT 1`, accountID, chatJID).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return exp, err
}

// ChatName returns a chat's stored name, "" when it has none or is not stored.
func (d *DB) ChatName(accountID, jid string) (string, error) {
	var name string
	err := d.sql.QueryRow(`SELECT name FROM chats WHERE account_id = ? AND jid = ?`, accountID, jid).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// UpsertChat sets a chat's name and last-message time (time never moves backwards).
// An empty name keeps the existing one.
func (d *DB) UpsertChat(accountID, jid, name string, lastMessageTS int64) error {
	_, err := d.sql.Exec(`INSERT INTO chats (account_id, jid, name, last_message_ts) VALUES (?,?,?,?)
		ON CONFLICT(account_id, jid) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE name END,
			last_message_ts = MAX(last_message_ts, excluded.last_message_ts)`, accountID, jid, name, lastMessageTS)
	return err
}

// NameChatIfUnnamed names a chat (creating it if needed) only when it has no name yet; for weak
// names such as push names, which must not replace a better one.
func (d *DB) NameChatIfUnnamed(accountID, jid, name string) error {
	_, err := d.sql.Exec(`INSERT INTO chats (account_id, jid, name) VALUES (?,?,?)
		ON CONFLICT(account_id, jid) DO UPDATE SET name = CASE WHEN name = '' THEN excluded.name ELSE name END`,
		accountID, jid, name)
	return err
}

// Story is a contact with recent status updates, for the stories strip.
type Story struct {
	SenderJID string
	Name      string // the sender's chat name; "" when there is none
	LastTS    int64  // newest status update, unix seconds
}

// RecentStories returns the distinct senders of the account's status updates at or after since (unix
// seconds), newest first. The user's own updates are left out.
func (d *DB) RecentStories(accountID string, since int64) ([]Story, error) {
	rows, err := d.sql.Query(`SELECT m.sender_jid, MAX(m.timestamp) AS ts,
		COALESCE((SELECT c.name FROM chats c WHERE c.account_id = m.account_id AND c.jid = m.sender_jid), '')
		FROM messages m WHERE m.account_id = ? AND m.chat_jid = ? AND m.from_me = 0 AND m.timestamp >= ?
		GROUP BY m.sender_jid ORDER BY ts DESC, m.sender_jid`, accountID, statusBroadcastJID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Story{}
	for rows.Next() {
		var s Story
		if err := rows.Scan(&s.SenderJID, &s.LastTS, &s.Name); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// maxStatusMessages caps one sender's stories returned by StatusMessages.
const maxStatusMessages = 50

// StatusMessages returns one sender's status updates in the account at or after since (unix
// seconds), oldest first; when there are more than maxStatusMessages, the newest ones.
func (d *DB) StatusMessages(accountID, senderJID string, since int64) ([]Message, error) {
	rows, err := d.sql.Query(`SELECT * FROM (SELECT `+messageCols+` FROM messages
		WHERE account_id = ? AND chat_jid = ? AND sender_jid = ? AND timestamp >= ?
		ORDER BY timestamp DESC, id DESC LIMIT ?) ORDER BY timestamp, id`,
		accountID, statusBroadcastJID, senderJID, since, maxStatusMessages)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ListChats returns the account's chats newest first, with snippet and derived unread count.
func (d *DB) ListChats(accountID string) ([]Chat, error) {
	rows, err := d.sql.Query(`SELECT c.jid, c.name, c.last_message_ts,
		COALESCE((SELECT body FROM messages m WHERE m.account_id = c.account_id AND m.chat_jid = c.jid
			ORDER BY timestamp DESC, id DESC LIMIT 1), ''),
		(SELECT COUNT(*) FROM messages m WHERE m.account_id = c.account_id AND m.chat_jid = c.jid
			AND m.from_me = 0 AND m.status <> 'read'), -- a literal, so messages_unread applies
		COALESCE((SELECT message_type FROM messages m WHERE m.account_id = c.account_id AND m.chat_jid = c.jid
			ORDER BY timestamp DESC, id DESC LIMIT 1), ''),
		COALESCE((SELECT sender_jid FROM messages m WHERE m.account_id = c.account_id AND m.chat_jid = c.jid
			ORDER BY timestamp DESC, id DESC LIMIT 1), ''),
		COALESCE((SELECT from_me FROM messages m WHERE m.account_id = c.account_id AND m.chat_jid = c.jid
			ORDER BY timestamp DESC, id DESC LIMIT 1), 0),
		COALESCE((SELECT media_meta FROM messages m WHERE m.account_id = c.account_id AND m.chat_jid = c.jid
			ORDER BY timestamp DESC, id DESC LIMIT 1), x'')
		FROM chats c WHERE c.account_id = ? ORDER BY c.last_message_ts DESC, c.jid`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.JID, &c.Name, &c.LastMessageTS, &c.Snippet, &c.Unread,
			&c.LastType, &c.LastSender, &c.LastFromMe, &c.LastMeta); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
