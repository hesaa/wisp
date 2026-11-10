// Package whatsapp runs several WhatsApp accounts at once (one whatsmeow client per paired phone),
// persists what they receive into the app store and fans events out to listeners such as the UI.
package whatsapp

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"math"
	"slices"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/store"
)

// Message types stored in store.Message.Type.
const (
	TypeText        = "text"
	TypeImage       = "image"
	TypeVideo       = "video"
	TypeAudio       = "audio"
	TypeDocument    = "document"
	TypeSticker     = "sticker"
	TypeUnsupported = "unsupported" // a message kind this app cannot show yet; kept, not dropped
)

// waClient is the part of *whatsmeow.Client the manager uses, so tests can fake it.
type waClient interface {
	Disconnect()
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	SendPresence(ctx context.Context, state types.Presence) error
	SetPrivacySetting(ctx context.Context, name types.PrivacySettingType, value types.PrivacySetting) (types.PrivacySettings, error)
	MarkRead(ctx context.Context, ids []types.MessageID, timestamp time.Time, chat, sender types.JID, receiptTypeExtra ...types.ReceiptType) error
	Logout(ctx context.Context) error
	UploadReader(ctx context.Context, plaintext io.Reader, tempFile io.ReadWriteSeeker, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	DownloadToFile(ctx context.Context, msg whatsmeow.DownloadableMessage, file whatsmeow.File) error
	ParseWebMessage(chatJID types.JID, webMsg *waWeb.WebMessageInfo) (*events.Message, error)
}

var _ waClient = (*whatsmeow.Client)(nil)

// newClient creates a whatsmeow client for dev whose events are handled under its account ID.
func (m *Manager) newClient(dev *wastore.Device) *whatsmeow.Client {
	cli := whatsmeow.NewClient(dev, waLog.Noop)
	// Held here, not read from dev.ID: whatsmeow clears dev.ID on a remote logout,
	// possibly before the LoggedOut event reaches us.
	var id atomic.Value
	id.Store("")
	if dev.ID != nil {
		id.Store(dev.ID.User)
	}
	cli.AddEventHandler(func(evt any) {
		if ps, ok := evt.(*events.PairSuccess); ok {
			id.Store(ps.ID.User)
		}
		acct := id.Load().(string)
		// shortcut: a failed store write is reported but the message is still acked, so WhatsApp won't
		// redeliver it; use SynchronousAck + EnableDecryptedEventBuffer + a success-status handler if that matters.
		if err := m.handleEvent(acct, cli, evt); err != nil {
			m.emit(ErrorEvent{AccountID: acct, Err: err})
		}
	})
	return cli
}

// handleEvent persists and forwards one whatsmeow event of account id. It runs on whatsmeow's
// goroutines (several at once per account); tests call it directly.
func (m *Manager) handleEvent(id string, cli waClient, evt any) error {
	if id == "" {
		return nil // still pairing: progress is reported on the QR channel
	}
	switch e := evt.(type) {
	case *events.Message:
		msg, ok, err := toStoreMessage(id, e)
		if err != nil || !ok {
			return err
		}
		name := ""
		if isDM(e.Info.Chat) && !e.Info.IsFromMe {
			name = e.Info.PushName
		}
		return m.persist(msg, name)
	case *events.Receipt:
		status := receiptStatus(e.Type)
		if status == "" {
			return nil
		}
		chat := e.Chat.String()
		if err := m.store.UpdateMessageStatus(id, chat, e.MessageIDs, status); err != nil {
			return err
		}
		m.emit(ReceiptEvent{AccountID: id, ChatJID: chat, IDs: e.MessageIDs, Status: status})
	case *events.HistorySync:
		return m.ingestHistory(id, cli, e)
	case *events.PairSuccess:
		m.add(id, cli)
		m.emit(ConnectionEvent{AccountID: id, State: StatePaired})
	case *events.Connected:
		m.emit(ConnectionEvent{AccountID: id, State: StateConnected})
	case *events.Disconnected:
		m.emit(ConnectionEvent{AccountID: id, State: StateDisconnected})
	case *events.StreamReplaced:
		m.emit(ConnectionEvent{AccountID: id, State: StateStreamReplaced})
	case *events.LoggedOut:
		m.remove(id, cli) // whatsmeow deletes the session itself
		m.emit(ConnectionEvent{AccountID: id, State: StateLoggedOut, Detail: e.Reason.String()})
	case *events.TemporaryBan:
		m.emit(ConnectionEvent{AccountID: id, State: StateFailed, Detail: e.String()})
	case *events.ConnectFailure:
		m.emit(ConnectionEvent{AccountID: id, State: StateFailed, Detail: e.Reason.String() + ": " + e.Message})
	case *events.ClientOutdated:
		m.emit(ConnectionEvent{AccountID: id, State: StateFailed, Detail: "client outdated"})
	}
	return nil
}

// persist stores msg, names its chat when chatName is known, and tells listeners.
func (m *Manager) persist(msg store.Message, chatName string) error {
	if err := m.store.InsertMessagesBatch([]store.Message{msg}); err != nil {
		return err
	}
	if chatName != "" {
		if err := m.store.UpsertChat(msg.AccountID, msg.ChatJID, chatName, msg.Timestamp); err != nil {
			return err
		}
	}
	m.emit(MessageEvent{AccountID: msg.AccountID, Message: msg})
	return nil
}

// ingestHistory stores a history-sync blob, flushing at most store.BatchSize messages at a time so
// the archive is never copied into one big slice. Unparseable messages are skipped and reported.
func (m *Manager) ingestHistory(id string, cli waClient, h *events.HistorySync) error {
	batch := make([]store.Message, 0, store.BatchSize)
	flush := func() error {
		err := m.store.InsertMessagesBatch(batch)
		batch = batch[:0]
		return err
	}
	var skipped int
	var firstErr error
	skip := func(err error) {
		skipped++
		firstErr = cmp.Or(firstErr, err)
	}
	for _, conv := range h.Data.GetConversations() {
		chat, err := types.ParseJID(conv.GetID())
		if err != nil {
			skip(err)
			continue
		}
		cutoff := unreadCutoff(conv)
		name := cmp.Or(conv.GetName(), conv.GetDisplayName())
		for _, hm := range conv.GetMessages() {
			evt, err := cli.ParseWebMessage(chat, hm.GetMessage())
			if err != nil {
				skip(err)
				continue
			}
			msg, ok, err := toStoreMessage(id, evt)
			if err != nil {
				skip(err)
			}
			if !ok {
				continue
			}
			if !msg.FromMe {
				if msg.Timestamp < cutoff {
					msg.Status = store.StatusRead
				}
				if name == "" && isDM(chat) {
					name = evt.Info.PushName
				}
			}
			batch = append(batch, msg)
			if len(batch) == store.BatchSize {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if chat != types.StatusBroadcastJID {
			if err := m.store.UpsertChat(id, chat.String(), name, int64(conv.GetConversationTimestamp())); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	m.emit(HistorySyncEvent{AccountID: id})
	if skipped > 0 {
		return fmt.Errorf("whatsapp: history sync skipped %d messages: %w", skipped, firstErr)
	}
	return nil
}

// unreadCutoff returns the timestamp from which incoming history messages count as unread: the
// phone only reports a per-chat unread count, which covers the newest incoming messages.
func unreadCutoff(conv *waHistorySync.Conversation) int64 {
	n := int(conv.GetUnreadCount())
	if n == 0 {
		return math.MaxInt64
	}
	if n >= len(conv.GetMessages()) {
		return math.MinInt64
	}
	newest := make([]int64, 0, n+1) // the n newest incoming timestamps, descending
	for _, hm := range conv.GetMessages() {
		if hm.GetMessage().GetKey().GetFromMe() {
			continue
		}
		ts := int64(hm.GetMessage().GetMessageTimestamp())
		i, _ := slices.BinarySearchFunc(newest, ts, func(e, t int64) int { return cmp.Compare(t, e) })
		if i < n {
			newest = slices.Insert(newest, i, ts)
			newest = newest[:min(len(newest), n)]
		}
	}
	if len(newest) < n {
		return math.MinInt64
	}
	return newest[n-1]
}

// toStoreMessage converts e for account id. ok is false for signalling that is not chat content
// (protocol messages such as revokes, edits and key shares; bare sender-key distribution).
func toStoreMessage(id string, e *events.Message) (msg store.Message, ok bool, err error) {
	msg = store.Message{
		ID: e.Info.ID, ChatJID: e.Info.Chat.String(), AccountID: id,
		SenderJID: e.Info.Sender.ToNonAD().String(), Timestamp: e.Info.Timestamp.Unix(),
		FromMe: e.Info.IsFromMe, Status: store.StatusReceived,
	}
	if msg.FromMe {
		msg.Status = store.StatusSent
	}
	var media proto.Message
	switch m := e.Message; {
	case m.GetConversation() != "":
		msg.Type, msg.Body = TypeText, m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		msg.Type, msg.Body = TypeText, m.GetExtendedTextMessage().GetText()
	case m.GetImageMessage() != nil:
		msg.Type, msg.Body, media = TypeImage, m.GetImageMessage().GetCaption(), m.GetImageMessage()
	case m.GetVideoMessage() != nil:
		msg.Type, msg.Body, media = TypeVideo, m.GetVideoMessage().GetCaption(), m.GetVideoMessage()
	case m.GetAudioMessage() != nil:
		msg.Type, media = TypeAudio, m.GetAudioMessage()
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		msg.Type, msg.Body, media = TypeDocument, cmp.Or(d.GetCaption(), d.GetFileName()), d
	case m.GetStickerMessage() != nil:
		msg.Type, media = TypeSticker, m.GetStickerMessage()
	case m.GetProtocolMessage() != nil, m.GetSenderKeyDistributionMessage() != nil:
		return msg, false, nil
	default:
		msg.Type = TypeUnsupported
	}
	if media != nil {
		if msg.MediaMeta, err = proto.Marshal(media); err != nil {
			return msg, false, fmt.Errorf("whatsapp: marshal media of %s: %w", msg.ID, err)
		}
	}
	return msg, true, nil
}

func receiptStatus(t types.ReceiptType) string {
	switch t {
	case types.ReceiptTypeDelivered:
		return store.StatusDelivered
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf, types.ReceiptTypePlayed:
		return store.StatusRead // read-self: the user read incoming messages on another device
	}
	return ""
}

func isDM(chat types.JID) bool {
	return chat.Server == types.DefaultUserServer || chat.Server == types.HiddenUserServer
}
