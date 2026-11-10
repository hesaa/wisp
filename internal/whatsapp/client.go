// Package whatsapp runs several WhatsApp accounts at once (one whatsmeow client per paired phone),
// persists what they receive into the app store and fans events out to listeners such as the UI.
package whatsapp

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"math"
	"runtime/debug"
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
	TypeRevoked     = "revoked"     // deleted for everyone; Body holds revokedBody
	TypeUnsupported = "unsupported" // a message kind this app cannot show yet; kept (as read), not dropped
)

const revokedBody = "This message was deleted"

// waClient is the part of *whatsmeow.Client (plus its device-store lookups) the manager uses, so
// tests can fake it.
type waClient interface {
	Disconnect()
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	SendPresence(ctx context.Context, state types.Presence) error
	SendChatPresence(ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error
	SetPrivacySetting(ctx context.Context, name types.PrivacySettingType, value types.PrivacySetting) (types.PrivacySettings, error)
	TryFetchPrivacySettings(ctx context.Context, ignoreCache bool) (*types.PrivacySettings, error)
	SetDefaultDisappearingTimer(ctx context.Context, timer time.Duration) error
	SetProxyAddress(addr string, opts ...whatsmeow.SetProxyOptions) error
	MarkRead(ctx context.Context, ids []types.MessageID, timestamp time.Time, chat, sender types.JID, receiptTypeExtra ...types.ReceiptType) error
	Logout(ctx context.Context) error
	UploadReader(ctx context.Context, plaintext io.Reader, tempFile io.ReadWriteSeeker, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	DownloadToFile(ctx context.Context, msg whatsmeow.DownloadableMessage, file whatsmeow.File) error
	ParseWebMessage(chatJID types.JID, webMsg *waWeb.WebMessageInfo) (*events.Message, error)
	PNForLID(ctx context.Context, lid types.JID) (types.JID, error) // empty JID when unknown
	ContactName(ctx context.Context, jid types.JID) (string, error) // address-book name, "" when unknown
	AllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error)
	GetGroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error)
}

// liveClient adds the device-store lookups to a real client.
type liveClient struct{ *whatsmeow.Client }

func (c liveClient) PNForLID(ctx context.Context, lid types.JID) (types.JID, error) {
	return c.Store.LIDs.GetPNForLID(ctx, lid)
}

func (c liveClient) ContactName(ctx context.Context, jid types.JID) (string, error) {
	info, err := c.Store.Contacts.GetContact(ctx, jid)
	return info.FullName, err
}

func (c liveClient) AllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	return c.Store.Contacts.GetAllContacts(ctx)
}

var _ waClient = liveClient{}

// newClient creates a whatsmeow client for dev whose events are handled under its account ID.
func (m *Manager) newClient(dev *wastore.Device) *whatsmeow.Client {
	cli := whatsmeow.NewClient(dev, waLog.Noop)
	if err := cli.SetProxyAddress(m.proxyAddress()); err != nil {
		panic(err) // unreachable: SetProxy keeps only addresses ValidateProxy accepted
	}
	// A message whose handler failed is not acked, so WhatsApp redelivers it; the buffer lets
	// whatsmeow decrypt it again (the signal session has already moved on).
	cli.EnableDecryptedEventBuffer = true
	// Held here, not read from dev.ID: whatsmeow clears dev.ID on a remote logout,
	// possibly before the LoggedOut event reaches us.
	var id atomic.Value
	id.Store("")
	if dev.ID != nil {
		id.Store(dev.ID.User)
	}
	c := liveClient{cli}
	cli.AddEventHandlerWithSuccessStatus(func(evt any) bool {
		if ps, ok := evt.(*events.PairSuccess); ok {
			id.Store(ps.ID.User)
		}
		return m.onEvent(context.Background(), id.Load().(string), c, evt)
	})
	return cli
}

// onEvent handles evt and reports whether whatsmeow may ack it. A message that could not be
// stored is not acked, so WhatsApp delivers it again; any failure is also sent as an ErrorEvent.
func (m *Manager) onEvent(ctx context.Context, id string, cli waClient, evt any) bool {
	err := m.handleEvent(ctx, id, cli, evt)
	if err == nil {
		return true
	}
	m.emit(ErrorEvent{AccountID: id, Err: err})
	_, isMessage := evt.(*events.Message)
	return !isMessage
}

// handleEvent persists and forwards one whatsmeow event of account id. It runs on whatsmeow's
// goroutines (several at once per account); tests call it directly.
func (m *Manager) handleEvent(ctx context.Context, id string, cli waClient, evt any) error {
	if id == "" {
		return nil // still pairing: progress is reported on the QR channel
	}
	switch e := evt.(type) {
	case *events.Message:
		info := e.Info
		var err error
		if info.MessageSource, err = pnSource(ctx, cli, info.MessageSource); err != nil {
			return err
		}
		if pm := e.Message.GetProtocolMessage(); pm != nil {
			return m.revise(ctx, id, cli, info, pm)
		}
		msg, ok, err := toStoreMessage(id, info, e.Message)
		if err != nil || !ok {
			return err
		}
		// MarkRead must address receipts the way the message arrived, not by the normalised key.
		msg.WireChatJID, msg.WireSenderJID = e.Info.Chat.String(), e.Info.Sender.ToNonAD().String()
		if isDM(info.Chat) && !info.IsFromMe {
			if err := m.nameDM(ctx, id, cli, info.Chat, info.PushName); err != nil {
				return err
			}
		}
		if info.Chat.Server == types.GroupServer {
			m.nameGroupOnce(id, cli, info.Chat)
		}
		return m.persist(msg)
	case *events.Receipt:
		status := receiptStatus(e.Type)
		if status == "" {
			return nil
		}
		src, err := pnSource(ctx, cli, e.MessageSource)
		if err != nil {
			return err
		}
		chat := src.Chat.String()
		if err := m.store.UpdateMessageStatus(id, chat, e.MessageIDs, status); err != nil {
			return err
		}
		m.emit(ReceiptEvent{AccountID: id, ChatJID: chat, IDs: e.MessageIDs, Status: status, FromMe: e.IsFromMe})
	case *events.MarkChatAsRead:
		return m.chatReadElsewhere(ctx, id, cli, e)
	case *events.JoinedGroup:
		return m.nameChat(id, e.JID, e.Name)
	case *events.GroupInfo:
		if e.Name != nil {
			return m.nameChat(id, e.JID, e.Name.Name)
		}
	case *events.PushNameSetting:
		m.emit(PushNameEvent{AccountID: id})
	case *events.HistorySync:
		err := m.ingestHistory(ctx, id, cli, e)
		// whatsmeow decoded the whole blob before this event; drop our handle (we are its only
		// reader) and return the spike to the OS so idle RSS recovers.
		e.Data = nil
		debug.FreeOSMemory()
		return err
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

// persist stores msg and, unless it is a redelivered duplicate, tells listeners.
func (m *Manager) persist(msg store.Message) error {
	isNew, err := m.store.InsertMessage(msg)
	if err == nil && isNew {
		m.emit(MessageEvent{AccountID: msg.AccountID, Message: msg})
	}
	return err
}

// chatReadElsewhere applies a chat marked read on another of the user's devices (app state): its
// incoming messages up to the newest one that device had seen (else up to when it was marked) are
// read. Marking a chat unread changes nothing here.
func (m *Manager) chatReadElsewhere(ctx context.Context, id string, cli waClient, e *events.MarkChatAsRead) error {
	if !e.Action.GetRead() {
		return nil
	}
	chat, err := toPN(ctx, cli, e.JID, types.EmptyJID)
	if err != nil {
		return err
	}
	until := e.Action.GetMessageRange().GetLastMessageTimestamp()
	if until <= 0 {
		until = e.Timestamp.Unix()
	}
	if until <= 0 {
		until = math.MaxInt64
	}
	if err := m.store.MarkChatReadUntil(id, chat.String(), until); err != nil {
		return err
	}
	m.emit(ChatReadEvent{AccountID: id, ChatJID: chat.String()})
	return nil
}

// nameChat records a group's name (joined, or renamed) and tells listeners.
func (m *Manager) nameChat(id string, chat types.JID, name string) error {
	if name == "" {
		return nil
	}
	if err := m.store.UpsertChat(id, chat.String(), name, 0); err != nil {
		return err
	}
	m.emit(ChatNamedEvent{AccountID: id, ChatJID: chat.String(), Name: name})
	return nil
}

// nameGroupOnce names an unnamed group chat from WhatsApp's group info: on the group's first message
// this run, off the message path, a few lookups at a time. A lookup that fails is not retried until
// the next run.
func (m *Manager) nameGroupOnce(id string, cli waClient, group types.JID) {
	key := id + " " + group.String()
	m.mu.Lock()
	asked := m.groupsAsked[key]
	m.groupsAsked[key] = true
	m.mu.Unlock()
	if asked {
		return
	}
	m.groupFetches.Go(func() {
		m.groupSlots <- struct{}{}
		defer func() { <-m.groupSlots }()
		name, err := m.store.ChatName(id, group.String())
		if err == nil && name == "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			var info *types.GroupInfo
			if info, err = cli.GetGroupInfo(ctx, group); err == nil {
				err = m.nameChat(id, group, info.Name)
			}
			cancel()
		}
		if err != nil {
			m.emit(ErrorEvent{AccountID: id, Err: fmt.Errorf("whatsapp: name of group %s: %w", group, err)})
		}
	})
}

// nameDM records the best name for a 1:1 chat: the address-book name always wins; a push name only
// names a chat that has none ("-" means no name, as in whatsmeow).
func (m *Manager) nameDM(ctx context.Context, id string, cli waClient, chat types.JID, pushName string) error {
	full, err := cli.ContactName(ctx, chat)
	if err != nil {
		return err
	}
	if full != "" {
		return m.store.UpsertChat(id, chat.String(), full, 0)
	}
	if pushName == "" || pushName == "-" {
		return nil
	}
	return m.store.NameChatIfUnnamed(id, chat.String(), pushName)
}

// revise applies an edit or a delete-for-everyone sent by the target message's own author; anything
// else (a forged edit, a group admin deleting someone else's message) changes nothing. Other
// protocol messages (key shares, history-sync notices, ...) are signalling and ignored.
func (m *Manager) revise(ctx context.Context, id string, cli waClient, info types.MessageInfo, pm *waE2E.ProtocolMessage) error {
	if pm.Type == nil {
		return nil // REVOKE is the zero value: never infer it from a missing type
	}
	var typ, body string
	switch pm.GetType() {
	case waE2E.ProtocolMessage_MESSAGE_EDIT:
		t, b, _, skip := content(pm.GetEditedMessage())
		if skip || t == TypeUnsupported {
			return nil
		}
		body = b
	case waE2E.ProtocolMessage_REVOKE:
		typ, body = TypeRevoked, revokedBody
	default:
		return nil
	}
	chat, target := info.Chat.String(), pm.GetKey().GetID()
	if ok, err := m.isAuthor(ctx, id, cli, info, chat, target); err != nil || !ok {
		return err
	}
	changed, err := m.store.ReviseMessage(id, chat, target, typ, body)
	if err == nil && changed {
		m.emit(RevisedEvent{AccountID: id, ChatJID: chat, ID: target})
	}
	return err
}

// isAuthor reports whether info's sender wrote the stored message target. Both sides are compared
// as phone-number JIDs (a stored LID is resolved now, its mapping may be newer than the message);
// the user's own messages, from any device, are the account's own number.
func (m *Manager) isAuthor(ctx context.Context, id string, cli waClient, info types.MessageInfo, chat, target string) (bool, error) {
	stored, err := m.store.MessageSender(id, chat, target)
	if err != nil || stored == "" {
		return false, err // not stored: nothing to revise
	}
	author, err := types.ParseJID(stored)
	if err == nil {
		author, err = toPN(ctx, cli, author, types.EmptyJID)
	}
	if err != nil {
		return false, err
	}
	editor := info.Sender.ToNonAD()
	if info.IsFromMe {
		editor = types.NewJID(id, types.DefaultUserServer)
	}
	return author.ToNonAD().String() == editor.String(), nil
}

// pnSource rewrites a 1:1 chat and the sender from LID to phone-number JIDs, so each contact is
// stored under one key. An LID with no known phone number is kept. Groups keep their @g.us chat.
func pnSource(ctx context.Context, cli waClient, src types.MessageSource) (types.MessageSource, error) {
	chatAlt := src.SenderAlt
	if src.IsFromMe {
		chatAlt = src.RecipientAlt
	}
	var err error
	if src.Chat, err = toPN(ctx, cli, src.Chat, chatAlt); err != nil {
		return src, err
	}
	src.Sender, err = toPN(ctx, cli, src.Sender, src.SenderAlt)
	return src, err
}

// toPN returns the phone-number JID for an LID jid (from alt when whatsmeow supplied it, else from
// the account's LID map); other JIDs are returned unchanged.
func toPN(ctx context.Context, cli waClient, jid, alt types.JID) (types.JID, error) {
	if jid.Server != types.HiddenUserServer {
		return jid, nil
	}
	if alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.ToNonAD(), nil
	}
	pn, err := cli.PNForLID(ctx, jid.ToNonAD())
	if err != nil || pn.IsEmpty() {
		return jid, err
	}
	return pn.ToNonAD(), nil
}

// ingestHistory stores a history-sync blob, flushing at most store.BatchSize messages at a time so
// the archive is never copied into one big slice. Unparseable messages are skipped and reported.
func (m *Manager) ingestHistory(ctx context.Context, id string, cli waClient, h *events.HistorySync) error {
	batch := make([]store.Message, 0, store.BatchSize)
	var skipped int
	var firstErr error
	// add queues one archived message, flushing the batch when full. It returns the parsed info
	// (nil when the message was skipped) and only store errors, which abort the sync.
	add := func(chat types.JID, wm *waWeb.WebMessageInfo, unreadFrom int64) (*types.MessageInfo, error) {
		evt, err := cli.ParseWebMessage(chat, wm)
		if err == nil {
			evt.Info.MessageSource, err = pnSource(ctx, cli, evt.Info.MessageSource)
		}
		var msg store.Message
		ok := false
		if err == nil {
			msg, ok, err = toStoreMessage(id, evt.Info, evt.Message)
		}
		if err != nil {
			skipped++
			firstErr = cmp.Or(firstErr, err)
		}
		if !ok {
			return nil, nil
		}
		if !msg.FromMe && msg.Timestamp < unreadFrom {
			msg.Status = store.StatusRead
		}
		batch = append(batch, msg)
		if len(batch) < store.BatchSize {
			return &evt.Info, nil
		}
		err = m.store.InsertMessagesBatch(batch)
		batch = batch[:0]
		return &evt.Info, err
	}
	for _, conv := range h.Data.GetConversations() {
		chat, err := types.ParseJID(conv.GetID())
		if err == nil {
			alt, _ := types.ParseJID(conv.GetPnJID()) // unusable alts are ignored by toPN
			chat, err = toPN(ctx, cli, chat, alt)
		}
		if err != nil {
			skipped++
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		cutoff := unreadCutoff(conv)
		pushName := ""
		for _, hm := range conv.GetMessages() {
			info, err := add(chat, hm.GetMessage(), cutoff)
			if err != nil {
				return err
			}
			if info != nil && !info.IsFromMe && pushName == "" {
				pushName = info.PushName
			}
		}
		if chat == types.StatusBroadcastJID {
			continue
		}
		name := cmp.Or(conv.GetName(), conv.GetDisplayName())
		if err := m.store.UpsertChat(id, chat.String(), name, int64(conv.GetConversationTimestamp())); err != nil {
			return err
		}
		if name == "" && isDM(chat) {
			if err := m.nameDM(ctx, id, cli, chat, pushName); err != nil {
				return err
			}
		}
	}
	for _, wm := range h.Data.GetStatusV3Messages() { // stories: stored, never a chat row
		if _, err := add(types.StatusBroadcastJID, wm, math.MinInt64); err != nil {
			return err
		}
	}
	if err := m.store.InsertMessagesBatch(batch); err != nil {
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

// toStoreMessage converts a message of account id. ok is false for what is not chat content
// (see content).
func toStoreMessage(id string, info types.MessageInfo, m *waE2E.Message) (msg store.Message, ok bool, err error) {
	typ, body, media, skip := content(m)
	if skip {
		return msg, false, nil
	}
	msg = store.Message{
		ID: info.ID, ChatJID: info.Chat.String(), AccountID: id,
		SenderJID: info.Sender.ToNonAD().String(), Timestamp: info.Timestamp.Unix(),
		Type: typ, Body: body, FromMe: info.IsFromMe, Status: store.StatusReceived,
	}
	if exp := contextInfo(m).GetExpiration(); exp > 0 { // disappearing messages: honoured here too
		msg.ExpiresAt = msg.Timestamp + int64(exp)
	}
	switch {
	case typ == TypeUnsupported:
		msg.Status = store.StatusRead // never counts as unread
	case msg.FromMe:
		msg.Status = store.StatusSent
	}
	if media != nil {
		if msg.MediaMeta, err = proto.Marshal(media); err != nil {
			return msg, false, fmt.Errorf("whatsapp: marshal media of %s: %w", msg.ID, err)
		}
	}
	return msg, true, nil
}

// content classifies a message: its stored type and body and the media sub-message to keep. skip is
// true for what is not chat content: protocol messages, bare sender-key distribution, reactions,
// poll votes, pins and keep-in-chat markers.
func content(m *waE2E.Message) (typ, body string, media proto.Message, skip bool) {
	switch {
	case m.GetConversation() != "":
		return TypeText, m.GetConversation(), nil, false
	case m.GetExtendedTextMessage() != nil:
		return TypeText, m.GetExtendedTextMessage().GetText(), nil, false
	case m.GetImageMessage() != nil:
		return TypeImage, m.GetImageMessage().GetCaption(), m.GetImageMessage(), false
	case m.GetVideoMessage() != nil:
		return TypeVideo, m.GetVideoMessage().GetCaption(), m.GetVideoMessage(), false
	case m.GetAudioMessage() != nil:
		return TypeAudio, "", m.GetAudioMessage(), false
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		return TypeDocument, cmp.Or(d.GetCaption(), d.GetFileName()), d, false
	case m.GetStickerMessage() != nil:
		return TypeSticker, "", m.GetStickerMessage(), false
	case m.GetProtocolMessage() != nil, m.GetSenderKeyDistributionMessage() != nil,
		m.GetReactionMessage() != nil, m.GetPollUpdateMessage() != nil,
		m.GetPinInChatMessage() != nil, m.GetKeepInChatMessage() != nil:
		return "", "", nil, true
	}
	return TypeUnsupported, "", nil, false
}

// contextInfo is the context info (quote, disappearing timer, ...) of a message's content; nil when
// it has none.
func contextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	switch {
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetContextInfo()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetContextInfo()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetContextInfo()
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage().GetContextInfo()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetContextInfo()
	case m.GetStickerMessage() != nil:
		return m.GetStickerMessage().GetContextInfo()
	}
	return nil
}

// disappearing makes an outgoing message disappear exp seconds after it is sent, as the chat's
// timer asks: the timer travels in the content's context info, so plain text becomes extended text.
func disappearing(m *waE2E.Message, exp uint32) {
	if m.Conversation != nil {
		m.ExtendedTextMessage, m.Conversation = &waE2E.ExtendedTextMessage{Text: m.Conversation}, nil
	}
	ci := &waE2E.ContextInfo{Expiration: proto.Uint32(exp)}
	switch {
	case m.ExtendedTextMessage != nil:
		m.ExtendedTextMessage.ContextInfo = ci
	case m.ImageMessage != nil:
		m.ImageMessage.ContextInfo = ci
	case m.VideoMessage != nil:
		m.VideoMessage.ContextInfo = ci
	case m.DocumentMessage != nil:
		m.DocumentMessage.ContextInfo = ci
	}
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
