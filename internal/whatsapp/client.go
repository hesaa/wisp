// Package whatsapp runs several WhatsApp accounts at once (one whatsmeow client per paired phone),
// persists what they receive into the app store and fans events out to listeners such as the UI.
package whatsapp

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

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
	SendPeerMessage(ctx context.Context, message *waE2E.Message) (whatsmeow.SendResponse, error)
	BuildHistorySyncRequest(lastKnownMessageInfo *types.MessageInfo, count int) *waE2E.Message
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
	GetProfilePictureInfo(ctx context.Context, jid types.JID, params *whatsmeow.GetProfilePictureParams) (*types.ProfilePictureInfo, error)
	GetUserInfo(ctx context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error)
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
		rawSender := info.Sender.ToNonAD()
		if rawSender.IsEmpty() && isDM(info.Chat) {
			rawSender = info.Chat.ToNonAD()
		}
		var pnJID string
		if rawSender.Server == types.HiddenUserServer {
			if info.SenderAlt.Server == types.DefaultUserServer && info.SenderAlt.User != "" {
				pnJID = info.SenderAlt.ToNonAD().String()
			} else {
				pn, err := cli.PNForLID(ctx, rawSender)
				if err == nil && !pn.IsEmpty() {
					pnJID = pn.ToNonAD().String()
				}
			}
		}
		var err error
		if info.MessageSource, err = pnSource(ctx, cli, info.MessageSource); err != nil {
			return err
		}
		logReceivedMessage(id, info, rawSender, pnJID, e.Message)
		if pm := e.Message.GetProtocolMessage(); pm != nil {
			return m.revise(ctx, id, cli, info, pm)
		}
		msg, ok, err := toStoreMessage(id, info, e.Message)
		if err != nil || !ok {
			return err
		}
		if err := m.creditQuote(ctx, cli, &msg); err != nil {
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
		if !info.IsFromMe && rawSender.User != "" {
			pushName := info.PushName
			if pushName == "-" || maskedName(pushName) {
				pushName = ""
			}
			p := store.Person{
				JID:      rawSender.String(),
				PNJID:    pnJID,
				PushName: pushName,
			}
			if err := m.upsertPeople(id, []store.Person{p}); err != nil {
				return err
			}
		}
		return m.persist(msg)
	case *events.Receipt:
		log.Printf("whatsapp [%s]: receipt recv type=%s chat=%s ids=%v from_me=%t", id, e.Type, e.Chat, e.MessageIDs, e.IsFromMe)
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
		log.Printf("whatsapp [%s]: mark_chat_read recv chat=%s read=%t ts=%d", id, e.JID, e.Action.GetRead(), e.Timestamp.Unix())
		return m.chatReadElsewhere(ctx, id, cli, e)
	case *events.JoinedGroup:
		log.Printf("whatsapp [%s]: joined_group recv jid=%s name=%q", id, e.JID, e.Name)
		return m.nameChat(id, e.JID, e.Name)
	case *events.GroupInfo:
		log.Printf("whatsapp [%s]: group_info recv jid=%s join=%v leave=%v name=%v", id, e.JID, e.Join, e.Leave, e.Name)
		if len(e.Join)+len(e.Leave) > 0 {
			m.peopleMu.Lock()
			delete(m.members, id+" "+e.JID.String())
			m.peopleMu.Unlock()
			m.emit(PeopleEvent{AccountID: id}) // who can be mentioned changed
		}
		if len(e.Leave) > 0 {
			userRemoved := false
			for _, j := range e.Leave {
				if j.User == id {
					userRemoved = true
					break
				}
			}
			if userRemoved {
				body := "You're no longer a member of this group"
				if e.Sender != nil && e.Sender.User != id && e.Sender.User != "" {
					removerName := e.Sender.User
					m.peopleMu.RLock()
					if p, ok := m.people[id+" "+e.Sender.String()]; ok {
						if p.PushName != "" {
							removerName = p.PushName
						} else if p.FullName != "" {
							removerName = p.FullName
						}
					}
					m.peopleMu.RUnlock()
					body = fmt.Sprintf("~ %s removed you", removerName)
				}
				ts := e.Timestamp.Unix()
				if ts <= 0 {
					ts = time.Now().Unix()
				}
				msg := store.Message{
					AccountID: id,
					ChatJID:   e.JID.String(),
					ID:        fmt.Sprintf("sys_%d", e.Timestamp.UnixNano()),
					Type:      "system",
					Body:      body,
					Timestamp: ts,
					Status:    store.StatusRead,
				}
				_, _ = m.store.InsertMessage(msg)
				m.emit(MessageEvent{AccountID: id, Message: msg})
			}
		}
		if e.Name != nil {
			return m.nameChat(id, e.JID, e.Name.Name)
		}
	case *events.PushNameSetting:
		log.Printf("whatsapp [%s]: push_name_setting recv", id)
		m.emit(PushNameEvent{AccountID: id})
	case *events.Picture:
		log.Printf("whatsapp [%s]: picture recv jid=%s", id, e.JID)
		jid := e.JID.ToNonAD().String()
		if err := m.store.SetAvatar(id, jid, "", 0); err != nil {
			return err
		}
		m.emit(AvatarEvent{AccountID: id, JID: jid})
	case *events.HistorySync:
		log.Printf("whatsapp [%s]: history_sync recv type=%s conversations=%d", id, e.Data.GetSyncType(), len(e.Data.GetConversations()))
		err := m.ingestHistory(ctx, id, cli, e)
		// whatsmeow decoded the whole blob before this event; drop our handle (we are its only
		// reader) and return the spike to the OS so idle RSS recovers.
		e.Data = nil
		debug.FreeOSMemory()
		return err
	case *events.PairSuccess:
		log.Printf("whatsapp [%s]: paired", id)
		m.add(id, cli)
		m.emit(ConnectionEvent{AccountID: id, State: StatePaired})
	case *events.Connected:
		log.Printf("whatsapp [%s]: connected", id)
		m.syncContacts(ctx, id, cli)
		m.emit(ConnectionEvent{AccountID: id, State: StateConnected})
		go m.refetchUnsupported(context.Background(), id, cli, time.Second)
	case *events.Disconnected:
		log.Printf("whatsapp [%s]: disconnected", id)
		m.emit(ConnectionEvent{AccountID: id, State: StateDisconnected})
	case *events.StreamReplaced:
		log.Printf("whatsapp [%s]: stream_replaced", id)
		m.emit(ConnectionEvent{AccountID: id, State: StateStreamReplaced})
	case *events.LoggedOut:
		log.Printf("whatsapp [%s]: logged_out reason=%s", id, e.Reason)
		m.remove(id, cli) // whatsmeow deletes the session itself
		m.emit(ConnectionEvent{AccountID: id, State: StateLoggedOut, Detail: e.Reason.String()})
	case *events.TemporaryBan:
		log.Printf("whatsapp [%s]: temporary_ban %s", id, e.String())
		m.emit(ConnectionEvent{AccountID: id, State: StateFailed, Detail: e.String()})
	case *events.ConnectFailure:
		log.Printf("whatsapp [%s]: connect_failure reason=%s: %s", id, e.Reason, e.Message)
		m.emit(ConnectionEvent{AccountID: id, State: StateFailed, Detail: e.Reason.String() + ": " + e.Message})
	case *events.ClientOutdated:
		log.Printf("whatsapp [%s]: client_outdated", id)
		m.emit(ConnectionEvent{AccountID: id, State: StateFailed, Detail: "client outdated"})
	default:
		log.Printf("whatsapp [%s]: unhandled_event type=%T", id, evt)
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

// syncContacts upserts contacts from AllContacts into people once per connect.
func (m *Manager) syncContacts(ctx context.Context, id string, cli waClient) {
	all, err := cli.AllContacts(ctx)
	if err != nil || len(all) == 0 {
		return
	}
	people := make([]store.Person, 0, len(all))
	for jid, c := range all {
		if c.FullName != "" && !maskedName(c.FullName) {
			people = append(people, store.Person{
				JID:      jid.ToNonAD().String(),
				FullName: c.FullName,
			})
		}
	}
	if len(people) > 0 {
		_ = m.upsertPeople(id, people)
	}
}

// refetchSetting records that an account's unsupported messages were asked for again.
const refetchSetting = "refetch_unsupported_v1"

// refetchUnsupported asks the phone, once per account, to send again the chats holding messages
// an older version stored as unsupported; the replies arrive as on-demand history syncs, whose
// copies replace them (see store insertChunk) or delete them (not chat content). Requests go one
// per gap apart; a failed send leaves the account to try again on the next connect.
func (m *Manager) refetchUnsupported(ctx context.Context, id string, cli waClient, gap time.Duration) {
	if done, _, err := m.store.GetSetting(id, refetchSetting); err != nil || done != "" {
		return
	}
	rows, err := m.store.UnsupportedTimelines(id)
	if err != nil {
		log.Printf("whatsapp [%s]: refetch unsupported: %v", id, err)
		return
	}
	reqs := refetchPlan(rows, 50, time.Now())
	log.Printf("whatsapp [%s]: refetch unsupported: %d requests", id, len(reqs))
	for i, r := range reqs {
		if i > 0 {
			time.Sleep(gap)
		}
		if _, err := cli.SendPeerMessage(ctx, cli.BuildHistorySyncRequest(&r.anchor, r.count)); err != nil {
			log.Printf("whatsapp [%s]: refetch unsupported: request %d of %d: %v", id, i+1, len(reqs), err)
			return
		}
		log.Printf("whatsapp [%s]: refetch request chat=%s anchor=%s count=%d", id, r.anchor.Chat, r.anchor.ID, r.count)
	}
	if err := m.store.SetSetting(id, refetchSetting, time.Now().Format(time.RFC3339)); err != nil {
		log.Printf("whatsapp [%s]: refetch unsupported: %v", id, err)
	}
}

type refetchRequest struct {
	anchor types.MessageInfo // the phone sends the count messages just before it
	count  int
}

// refetchPlan covers every unsupported message of rows (by chat, newest first) with requests of
// at most window messages each. A request is anchored on the message just newer than the first
// unsupported one it covers; when that one is the chat's newest there is none, and the anchor is a
// made-up message at now (shortcut: the phone may ignore an anchor it doesn't know; then those
// stay unsupported).
func refetchPlan(rows []store.Message, window int, now time.Time) []refetchRequest {
	var out []refetchRequest
	for start := 0; start < len(rows); {
		end := start
		for end < len(rows) && rows[end].ChatJID == rows[start].ChatJID {
			end++
		}
		chat := rows[start:end]
		wire, err := types.ParseJID(cmp.Or(chat[0].WireChatJID, chat[0].ChatJID))
		for i := 0; err == nil && i < len(chat); i++ {
			if chat[i].Type != TypeUnsupported {
				continue
			}
			anchor := types.MessageInfo{MessageSource: types.MessageSource{Chat: wire}}
			if i == 0 {
				anchor.ID, anchor.Timestamp = "3EB0"+strings.ToUpper(strconv.FormatInt(now.UnixNano(), 16)), now
			} else {
				a := chat[i-1]
				anchor.ID, anchor.IsFromMe, anchor.Timestamp = a.ID, a.FromMe, time.Unix(a.Timestamp, 0)
			}
			out = append(out, refetchRequest{anchor: anchor, count: window})
			i += window - 1 // the rest of this window is covered
		}
		start = end
	}
	return out
}

// nameDM records the best name for a 1:1 chat: the address-book name always wins; a push name only
// names a chat that has none ("-" means no name, as in whatsmeow). Masked names are never stored.
func (m *Manager) nameDM(ctx context.Context, id string, cli waClient, chat types.JID, pushName string) error {
	full, err := cli.ContactName(ctx, chat)
	if err != nil {
		return err
	}
	if full != "" && !maskedName(full) {
		return m.store.UpsertChat(id, chat.String(), full, 0)
	}
	if pushName == "" || pushName == "-" || maskedName(pushName) {
		return nil
	}
	return m.store.NameChatIfUnnamed(id, chat.String(), pushName)
}

// maskedName reports whether s is a masked number (+, then only digits, spaces and at least one
// of •, ·, *, ., ∙).
func maskedName(s string) bool {
	if !strings.HasPrefix(s, "+") {
		return false
	}
	hasMask := false
	for _, r := range s[1:] {
		switch r {
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', ' ':
			// valid digit or space
		case '•', '·', '*', '.', '∙':
			hasMask = true
		default:
			return false
		}
	}
	return hasMask
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
		log.Printf("whatsapp [%s]: revise message_edit id=%s target_key=%s new_body=%q", id, info.ID, pm.GetKey().GetID(), body)
	case waE2E.ProtocolMessage_REVOKE:
		typ, body = TypeRevoked, revokedBody
		log.Printf("whatsapp [%s]: revise revoke id=%s target_key=%s", id, info.ID, pm.GetKey().GetID())
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
			if err == nil { // not chat content: drop a copy an older version stored as unsupported
				return nil, m.store.DeleteUnsupported(id, evt.Info.Chat.String(), evt.Info.ID)
			}
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
	var people []store.Person
	for _, conv := range h.Data.GetConversations() {
		if convID, pnJID := conv.GetID(), conv.GetPnJID(); convID != "" && pnJID != "" {
			cID, err1 := types.ParseJID(convID)
			pJID, err2 := types.ParseJID(pnJID)
			if err1 == nil && err2 == nil {
				people = append(people, store.Person{
					JID:   cID.ToNonAD().String(),
					PNJID: pJID.ToNonAD().String(),
				})
			}
		}
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
		if maskedName(name) {
			name = ""
		}
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
	if len(people) > 0 {
		if err := m.upsertPeople(id, people); err != nil {
			return err
		}
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
	m = unwrap(m) // so contextInfo sees the mentions and quote of wrapped content too
	typ, body, media, skip := content(m)
	if skip {
		return msg, false, nil
	}
	msg = store.Message{
		ID: info.ID, ChatJID: info.Chat.String(), AccountID: id,
		SenderJID: info.Sender.ToNonAD().String(), Timestamp: info.Timestamp.Unix(),
		Type: typ, Body: body, FromMe: info.IsFromMe, Status: store.StatusReceived,
	}
	if ci := contextInfo(m); ci != nil {
		if mentioned := ci.GetMentionedJID(); len(mentioned) > 0 {
			var valid []string
			for _, j := range mentioned {
				if j != "" {
					valid = append(valid, j)
				}
			}
			if len(valid) > 0 {
				msg.Mentions = strings.Join(valid, " ")
			}
		}
		if quoted := ci.GetStanzaID(); quoted != "" {
			msg.QuotedID = quoted
			// shortcut: a DM quote without a participant is credited to the chat partner, wrong for a
			// quote of our own message; look the quoted message up in the store if that ever shows.
			msg.QuotedSender = cmp.Or(ci.GetParticipant(), info.Chat.String())
			if qm := ci.GetQuotedMessage(); qm != nil {
				msg.QuotedType, msg.QuotedBody, _, _ = content(qm)
			}
		}
		if exp := ci.GetExpiration(); exp > 0 { // disappearing messages: honoured here too
			msg.ExpiresAt = msg.Timestamp + int64(exp)
		}
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

// creditQuote names the sender of the message msg quotes by the key senders are stored under: as the
// stored original has it, else the quote's participant with an LID mapped to its phone number.
func (m *Manager) creditQuote(ctx context.Context, cli waClient, msg *store.Message) error {
	if msg.QuotedID == "" {
		return nil
	}
	stored, err := m.store.MessageSender(msg.AccountID, msg.ChatJID, msg.QuotedID)
	if err != nil || stored != "" {
		msg.QuotedSender = cmp.Or(stored, msg.QuotedSender)
		return err
	}
	jid, err := types.ParseJID(msg.QuotedSender)
	if err != nil {
		return nil // kept as received
	}
	pn, err := toPN(ctx, cli, jid, types.EmptyJID)
	msg.QuotedSender = pn.ToNonAD().String()
	return err
}

// content classifies a message: its stored type and body and the media sub-message to keep. skip is
// true for what is not chat content: protocol messages, bare sender-key distribution, reactions,
// poll votes, pins and keep-in-chat markers.
func content(m *waE2E.Message) (typ, body string, media proto.Message, skip bool) {
	m = unwrap(m)
	if m == nil {
		return "", "", nil, true
	}
	if t, b, ok := described(m); ok {
		return TypeText, cmp.Or(b, t), nil, false
	}
	switch {
	case m.GetPtvMessage() != nil: // round video note
		return TypeVideo, m.GetPtvMessage().GetCaption(), markVideoNote(m.GetPtvMessage()), false
	case m.GetInstantImageMessage() != nil:
		return TypeImage, m.GetInstantImageMessage().GetCaption(), m.GetInstantImageMessage(), false
	case m.GetEncReactionMessage() != nil, m.GetEncEventResponseMessage() != nil, m.GetEncCommentMessage() != nil,
		m.GetSecretEncryptedMessage() != nil, m.GetAlbumMessage() != nil, m.GetPollAddOptionMessage() != nil,
		m.GetScheduledCallEditMessage() != nil, m.GetMessageHistoryBundle() != nil, m.GetMessageHistoryNotice() != nil,
		m.GetStickerSyncRmrMessage() != nil:
		return "signal", "", nil, true // encrypted votes/reactions, album headers, history notices: not chat content
	}
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
	case m.GetReactionMessage() != nil:
		return "reaction", m.GetReactionMessage().GetText(), nil, true
	case m.GetProtocolMessage() != nil:
		return "protocol", m.GetProtocolMessage().GetType().String(), nil, true
	case m.GetPollUpdateMessage() != nil:
		return "poll_update", "", nil, true
	case m.GetSenderKeyDistributionMessage() != nil:
		return "sender_key", "", nil, true
	case m.GetPinInChatMessage() != nil:
		return "pin", "", nil, true
	case m.GetKeepInChatMessage() != nil:
		return "keep_in_chat", "", nil, true
	}
	return TypeUnsupported, "", nil, false
}

// videoNoteField marks a stored VideoMessage as a round video note. VideoMessage has no field
// for that, so it rides as an unknown field (high number, far from WhatsApp's), which proto keeps.
const videoNoteField protowire.Number = 1 << 28

// markVideoNote returns a copy of v marked as a video note.
func markVideoNote(v *waE2E.VideoMessage) *waE2E.VideoMessage {
	v = proto.Clone(v).(*waE2E.VideoMessage)
	r := v.ProtoReflect()
	r.SetUnknown(protowire.AppendVarint(protowire.AppendTag(r.GetUnknown(), videoNoteField, protowire.VarintType), 1))
	return v
}

// IsVideoNote reports whether a stored VideoMessage is a round video note, and drops the marker so
// v is safe to send on.
func IsVideoNote(v *waE2E.VideoMessage) bool {
	r := v.ProtoReflect()
	b, kept, found := r.GetUnknown(), protoreflect.RawFields(nil), false
	for len(b) > 0 {
		num, _, n := protowire.ConsumeField(b)
		if n < 0 {
			return found // malformed: left as is
		}
		if num == videoNoteField {
			found = true
		} else {
			kept = append(kept, b[:n]...)
		}
		b = b[n:]
	}
	if found {
		r.SetUnknown(kept)
	}
	return found
}

// unwrap strips the FutureProofMessage wrappers whatsmeow leaves in place (quoted and edited
// content, mentions, spoilers, album children, ...), returning the message they carry.
func unwrap(m *waE2E.Message) *waE2E.Message {
	for i := 0; m != nil && i < 8; i++ { // bounded: a forged wrapper chain must not spin
		var inner *waE2E.Message
		for _, w := range []*waE2E.FutureProofMessage{
			m.GetEphemeralMessage(), m.GetViewOnceMessage(), m.GetViewOnceMessageV2(), m.GetViewOnceMessageV2Extension(),
			m.GetDocumentWithCaptionMessage(), m.GetEditedMessage(), m.GetGroupMentionedMessage(), m.GetBotInvokeMessage(),
			m.GetLottieStickerMessage(), m.GetStatusMentionMessage(), m.GetAssociatedChildMessage(),
			m.GetGroupStatusMentionMessage(), m.GetGroupStatusMessage(), m.GetGroupStatusMessageV2(),
			m.GetSpoilerMessage(), m.GetAudioStickerMessage(), m.GetPollCreationMessageV4(),
			m.GetEventCoverImage(), m.GetPollCreationOptionImageMessage(),
		} {
			if inner = w.GetMessage(); inner != nil {
				break
			}
		}
		if inner == nil {
			if inner = m.GetDeviceSentMessage().GetMessage(); inner == nil {
				return m
			}
		}
		m = inner
	}
	return m
}

// described labels message kinds the app shows as plain text: the label alone, or "label: detail".
func described(m *waE2E.Message) (label, body string, ok bool) {
	with := func(label, detail string) (string, string, bool) {
		if detail == "" {
			return label, "", true
		}
		return label, label + ": " + detail, true
	}
	poll := cmp.Or(m.GetPollCreationMessage(), m.GetPollCreationMessageV2(), m.GetPollCreationMessageV3(),
		m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6())
	switch {
	case m.GetLocationMessage() != nil:
		l := m.GetLocationMessage()
		return with("Location", cmp.Or(l.GetName(), l.GetAddress()))
	case m.GetLiveLocationMessage() != nil:
		return with("Live location", m.GetLiveLocationMessage().GetCaption())
	case m.GetRequestLocationMessage() != nil:
		return with("Location request", "")
	case m.GetContactMessage() != nil:
		return with("Contact", m.GetContactMessage().GetDisplayName())
	case m.GetContactsArrayMessage() != nil:
		c := m.GetContactsArrayMessage()
		return with("Contacts", cmp.Or(c.GetDisplayName(), fmt.Sprint(len(c.GetContacts()))))
	case poll != nil:
		return with("Poll", poll.GetName())
	case m.GetPollResultSnapshotMessage() != nil:
		return with("Poll results", m.GetPollResultSnapshotMessage().GetName())
	case m.GetPollResultSnapshotMessageV3() != nil:
		return with("Poll results", m.GetPollResultSnapshotMessageV3().GetName())
	case m.GetEventMessage() != nil:
		return with("Event", m.GetEventMessage().GetName())
	case m.GetCallLogMesssage() != nil:
		if m.GetCallLogMesssage().GetIsVideo() {
			return with("Video call", "")
		}
		return with("Voice call", "")
	case m.GetBcallMessage() != nil:
		return with("Call", "")
	case m.GetScheduledCallCreationMessage() != nil:
		return with("Scheduled call", m.GetScheduledCallCreationMessage().GetTitle())
	case m.GetGroupInviteMessage() != nil:
		return with("Group invite", m.GetGroupInviteMessage().GetGroupName())
	case m.GetNewsletterAdminInviteMessage() != nil:
		return with("Channel admin invite", m.GetNewsletterAdminInviteMessage().GetNewsletterName())
	case m.GetStickerPackMessage() != nil:
		return with("Sticker pack", m.GetStickerPackMessage().GetName())
	case m.GetRequestPhoneNumberMessage() != nil:
		return with("Phone number request", "")
	case m.GetProductMessage() != nil:
		return with("Product", m.GetProductMessage().GetProduct().GetTitle())
	case m.GetOrderMessage() != nil:
		return with("Order", m.GetOrderMessage().GetOrderTitle())
	case m.GetSendPaymentMessage() != nil, m.GetRequestPaymentMessage() != nil, m.GetInvoiceMessage() != nil,
		m.GetPaymentInviteMessage() != nil, m.GetPaymentReminderMessage() != nil, m.GetSplitPaymentMessage() != nil:
		return with("Payment", "")
	// business messages: their visible text, else a label
	case m.GetButtonsMessage() != nil:
		return "Message", m.GetButtonsMessage().GetContentText(), true
	case m.GetTemplateMessage() != nil:
		t := m.GetTemplateMessage()
		return "Message", cmp.Or(t.GetHydratedTemplate().GetHydratedContentText(), t.GetHydratedFourRowTemplate().GetHydratedContentText()), true
	case m.GetListMessage() != nil:
		l := m.GetListMessage()
		return "Message", cmp.Or(l.GetDescription(), l.GetTitle()), true
	case m.GetInteractiveMessage() != nil:
		return "Message", m.GetInteractiveMessage().GetBody().GetText(), true
	case m.GetButtonsResponseMessage() != nil:
		return "Reply", m.GetButtonsResponseMessage().GetSelectedDisplayText(), true
	case m.GetTemplateButtonReplyMessage() != nil:
		return "Reply", m.GetTemplateButtonReplyMessage().GetSelectedDisplayText(), true
	case m.GetListResponseMessage() != nil:
		return "Reply", m.GetListResponseMessage().GetTitle(), true
	case m.GetInteractiveResponseMessage() != nil:
		return "Reply", m.GetInteractiveResponseMessage().GetBody().GetText(), true
	case m.GetHighlyStructuredMessage() != nil:
		return "Message", "", true
	}
	return "", "", false
}

// logReceivedMessage logs all data received for an incoming WhatsApp message for debug purposes.
func logReceivedMessage(accountID string, info types.MessageInfo, sender types.JID, pnJID string, m *waE2E.Message) {
	typ, body, _, _ := content(m)
	var raw string
	if m != nil {
		if b, err := protojson.Marshal(m); err == nil {
			raw = string(b)
		} else {
			raw = m.String()
		}
	}
	var ts int64
	if !info.Timestamp.IsZero() {
		ts = info.Timestamp.Unix()
	}
	log.Printf("whatsapp [%s]: message recv id=%s chat=%s sender=%s pn=%s push=%q from_me=%t ts=%d type=%s body=%q raw=%s",
		accountID, info.ID, info.Chat, sender, pnJID, info.PushName, info.IsFromMe, ts, typ, body, raw)
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

// contextOf returns the context info of an outgoing message's content, creating it; plain text
// becomes extended text to carry one. It is nil for content that has none.
func contextOf(m *waE2E.Message) *waE2E.ContextInfo {
	if m.Conversation != nil {
		m.ExtendedTextMessage, m.Conversation = &waE2E.ExtendedTextMessage{Text: m.Conversation}, nil
	}
	var ci **waE2E.ContextInfo
	switch {
	case m.ExtendedTextMessage != nil:
		ci = &m.ExtendedTextMessage.ContextInfo
	case m.ImageMessage != nil:
		ci = &m.ImageMessage.ContextInfo
	case m.VideoMessage != nil:
		ci = &m.VideoMessage.ContextInfo
	case m.DocumentMessage != nil:
		ci = &m.DocumentMessage.ContextInfo
	default:
		return nil
	}
	if *ci == nil {
		*ci = &waE2E.ContextInfo{}
	}
	return *ci
}

// disappearing makes an outgoing message disappear exp seconds after it is sent, as the chat's
// timer asks.
func disappearing(m *waE2E.Message, exp uint32) {
	if ci := contextOf(m); ci != nil {
		ci.Expiration = proto.Uint32(exp)
	}
}

// quote makes the outgoing m a reply to the stored message reply; nil: not a reply.
func quote(m *waE2E.Message, reply *store.Message) {
	if reply == nil {
		return
	}
	ci := contextOf(m)
	if ci == nil {
		return
	}
	ci.StanzaID = proto.String(reply.ID)
	// addressed the way the original arrived (an LID or a phone number), as read receipts are
	ci.Participant = proto.String(cmp.Or(reply.WireSenderJID, reply.SenderJID))
	ci.QuotedMessage = quotedMessage(reply)
}

// quotedMessage rebuilds the content of a stored message for a quote: media from its stored
// sub-message, anything else as its text.
func quotedMessage(r *store.Message) *waE2E.Message {
	q := &waE2E.Message{}
	var media proto.Message
	switch r.Type {
	case TypeImage:
		q.ImageMessage = &waE2E.ImageMessage{}
		media = q.ImageMessage
	case TypeVideo:
		q.VideoMessage = &waE2E.VideoMessage{}
		media = q.VideoMessage
	case TypeAudio:
		q.AudioMessage = &waE2E.AudioMessage{}
		media = q.AudioMessage
	case TypeDocument:
		q.DocumentMessage = &waE2E.DocumentMessage{}
		media = q.DocumentMessage
	case TypeSticker:
		q.StickerMessage = &waE2E.StickerMessage{}
		media = q.StickerMessage
	}
	if media == nil || len(r.MediaMeta) == 0 || proto.Unmarshal(r.MediaMeta, media) != nil {
		return &waE2E.Message{Conversation: proto.String(r.Body)}
	}
	if q.VideoMessage != nil && IsVideoNote(q.VideoMessage) { // quoted as what it is, marker dropped
		q.PtvMessage, q.VideoMessage = q.VideoMessage, nil
	}
	return q
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
