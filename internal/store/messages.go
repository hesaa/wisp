package store

import "fmt"

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
}

// Chat is a sidebar row.
type Chat struct {
	JID           string
	Name          string
	LastMessageTS int64
	Snippet       string
	Unread        int
}

// InsertMessagesBatch stores msgs in transactions of BatchSize rows. Messages already
// stored (same account, chat, id) are ignored. Chats are created/bumped as needed
// (never for status@broadcast).
func (d *DB) InsertMessagesBatch(msgs []Message) error {
	for start := 0; start < len(msgs); start += BatchSize {
		end := min(start+BatchSize, len(msgs))
		if err := d.insertChunk(msgs[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) insertChunk(msgs []Message) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(`INSERT OR IGNORE INTO messages
		(account_id, chat_jid, id, sender_jid, timestamp, message_type, body, media_path, status, from_me, media_meta)
		VALUES (?,?,?,?,?,?,?,?,?,?,COALESCE(?, x''))`) // COALESCE: a nil []byte binds as NULL
	if err != nil {
		return err
	}
	defer ins.Close()
	chat, err := tx.Prepare(`INSERT INTO chats (account_id, jid, last_message_ts) VALUES (?,?,?)
		ON CONFLICT(account_id, jid) DO UPDATE SET last_message_ts = MAX(last_message_ts, excluded.last_message_ts)`)
	if err != nil {
		return err
	}
	defer chat.Close()
	for _, m := range msgs {
		if _, err := ins.Exec(m.AccountID, m.ChatJID, m.ID, m.SenderJID, m.Timestamp, m.Type, m.Body, m.MediaPath, m.Status, m.FromMe, m.MediaMeta); err != nil {
			return err
		}
		if m.ChatJID == statusBroadcastJID {
			continue
		}
		if _, err := chat.Exec(m.AccountID, m.ChatJID, m.Timestamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetChatMessages returns one page (0-based) of a chat, newest first. A page past the end is empty.
func (d *DB) GetChatMessages(accountID, chatJID string, page int) ([]Message, error) {
	if page < 0 {
		page = 0
	}
	rows, err := d.sql.Query(`SELECT id, chat_jid, account_id, sender_jid, timestamp, message_type, body, media_path, status, from_me, media_meta
		FROM messages WHERE account_id = ? AND chat_jid = ?
		ORDER BY timestamp DESC, id DESC LIMIT ? OFFSET ?`, accountID, chatJID, PageSize, page*PageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.AccountID, &m.SenderJID, &m.Timestamp, &m.Type, &m.Body, &m.MediaPath, &m.Status, &m.FromMe, &m.MediaMeta); err != nil {
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

// MarkChatRead marks all incoming messages in the chat as read.
func (d *DB) MarkChatRead(accountID, chatJID string) error {
	_, err := d.sql.Exec(`UPDATE messages SET status = ? WHERE account_id = ? AND chat_jid = ? AND from_me = 0 AND status <> ?`,
		StatusRead, accountID, chatJID, StatusRead)
	return err
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

// ListChats returns the account's chats newest first, with snippet and derived unread count.
func (d *DB) ListChats(accountID string) ([]Chat, error) {
	rows, err := d.sql.Query(`SELECT c.jid, c.name, c.last_message_ts,
		COALESCE((SELECT body FROM messages m WHERE m.account_id = c.account_id AND m.chat_jid = c.jid
			ORDER BY timestamp DESC, id DESC LIMIT 1), ''),
		(SELECT COUNT(*) FROM messages m WHERE m.account_id = c.account_id AND m.chat_jid = c.jid
			AND m.from_me = 0 AND m.status <> ?)
		FROM chats c WHERE c.account_id = ? ORDER BY c.last_message_ts DESC, c.jid`, StatusRead, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.JID, &c.Name, &c.LastMessageTS, &c.Snippet, &c.Unread); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
