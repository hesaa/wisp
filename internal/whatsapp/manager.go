package whatsapp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/media"
	"wa-go/internal/store"
)

// Connection states reported in ConnectionEvent.State.
const (
	StatePaired         = "paired"
	StateConnected      = "connected"
	StateDisconnected   = "disconnected"
	StateStreamReplaced = "stream_replaced" // another client took over this session
	StateLoggedOut      = "logged_out"      // unregistered; the phone must be paired again
	StateFailed         = "failed"          // temporary ban, connect failure or outdated client; see Detail
)

// ErrUnknownAccount is returned for an AccountID that has no registered account.
var ErrUnknownAccount = errors.New("whatsapp: unknown account")

// Events passed to listeners. Every one carries the AccountID (the paired phone number) it belongs to.
type (
	// MessageEvent: a message was stored (incoming, sent from another of the user's devices, or sent here).
	MessageEvent struct {
		AccountID string
		Message   store.Message
	}
	// ReceiptEvent: the stored status of IDs in ChatJID moved forward to Status.
	ReceiptEvent struct {
		AccountID, ChatJID string
		IDs                []string
		Status             string
	}
	// RevisedEvent: stored message ID in ChatJID was edited or deleted for everyone; reload it.
	RevisedEvent struct{ AccountID, ChatJID, ID string }
	// HistorySyncEvent: a history-sync blob was stored; reload the account's chat list.
	HistorySyncEvent struct{ AccountID string }
	// ConnectionEvent: the account's connection state changed (State* constants).
	ConnectionEvent struct{ AccountID, State, Detail string }
	// ErrorEvent: an incoming event could not be fully handled (for example a store write failed).
	ErrorEvent struct {
		AccountID string
		Err       error
	}
)

// msgStore is the part of *store.DB the manager writes to.
type msgStore interface {
	InsertMessagesBatch([]store.Message) error
	InsertMessage(store.Message) (bool, error)
	UpsertChat(accountID, jid, name string, lastMessageTS int64) error
	NameChatIfUnnamed(accountID, jid, name string) error
	UpdateMessageStatus(accountID, chatJID string, ids []string, status string) error
	ReviseMessage(accountID, chatJID, id, msgType, body string) error
	SetMediaPath(accountID, chatJID, id, path string) error
}

// Manager runs every paired account. It is safe for concurrent use and never touches the UI.
// Outbound calls block on the network, so call them off the UI thread.
type Manager struct {
	store     msgStore
	media     *media.Cache
	container *sqlstore.Container

	mu        sync.RWMutex // guards accounts and listeners; never held while calling whatsmeow or a listener
	accounts  map[string]waClient
	listeners []func(evt any)
}

// New opens (creating if needed) the whatsmeow session database at sessionPath, which holds every
// paired device. Call Start to connect them.
func New(ctx context.Context, sessionPath string, db *store.DB, cache *media.Cache) (*Manager, error) {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)") // sqlstore refuses to run without it
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	container, err := sqlstore.New(ctx, "sqlite", "file:"+sessionPath+"?"+q.Encode(), nil) // nil: no-op logger
	if err != nil {
		return nil, fmt.Errorf("whatsapp: open sessions: %w", err)
	}
	return &Manager{store: db, media: cache, container: container, accounts: map[string]waClient{}}, nil
}

// Start creates a client for every saved device and connects them concurrently. An account whose
// connection fails stays registered and its error is returned (joined with the others).
func (m *Manager) Start(ctx context.Context) error {
	devices, err := m.container.GetAllDevices(ctx)
	if err != nil {
		return fmt.Errorf("whatsapp: load sessions: %w", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(devices))
	for i, dev := range devices {
		cli := m.newClient(dev)
		cli.InitialAutoReconnect = true // app started offline: keep retrying in the background
		m.add(dev.ID.User, liveClient{cli})
		wg.Go(func() {
			if err := cli.Connect(); err != nil {
				errs[i] = fmt.Errorf("whatsapp: connect %s: %w", dev.ID.User, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Pair starts linking a new phone. Render each item whose Event is whatsmeow.QRChannelEventCode as a
// QR code of Code (it is replaced after Timeout). The channel then ends with whatsmeow.QRChannelSuccess
// (the account is registered and a StatePaired ConnectionEvent follows), QRChannelTimeout or an error
// item. Read it off the UI thread.
func (m *Manager) Pair(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	cli := m.newClient(m.container.NewDevice())
	qr, err := cli.GetQRChannel(ctx)
	if err != nil {
		return nil, err
	}
	if err := cli.Connect(); err != nil {
		return nil, fmt.Errorf("whatsapp: connect for pairing: %w", err)
	}
	return qr, nil
}

// AddListener registers fn to receive every event (MessageEvent, ReceiptEvent, RevisedEvent,
// HistorySyncEvent, ConnectionEvent, ErrorEvent). fn runs on whatsmeow's background goroutines (or on the goroutine
// that called SendText/SendMedia/Logout), possibly several at once: it must not block, and UI work
// must be marshalled onto the UI thread (walk: Synchronize).
func (m *Manager) AddListener(fn func(evt any)) {
	m.mu.Lock()
	m.listeners = append(m.listeners, fn)
	m.mu.Unlock()
}

func (m *Manager) emit(evt any) {
	m.mu.RLock()
	ls := m.listeners
	m.mu.RUnlock()
	for _, fn := range ls {
		fn(evt)
	}
}

// Accounts returns the registered account IDs, sorted.
func (m *Manager) Accounts() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Sorted(maps.Keys(m.accounts))
}

func (m *Manager) add(id string, cli waClient) {
	m.mu.Lock()
	m.accounts[id] = cli
	m.mu.Unlock()
}

// remove unregisters id unless a newer client (a re-pairing) has replaced cli.
func (m *Manager) remove(id string, cli waClient) {
	m.mu.Lock()
	if m.accounts[id] == cli {
		delete(m.accounts, id)
	}
	m.mu.Unlock()
}

func (m *Manager) client(id string) (waClient, error) {
	m.mu.RLock()
	cli, ok := m.accounts[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownAccount, id)
	}
	return cli, nil
}

// SendText sends text to chatJID from accountID and stores it as sent.
func (m *Manager) SendText(ctx context.Context, accountID, chatJID, text string) (store.Message, error) {
	cli, err := m.client(accountID)
	if err != nil {
		return store.Message{}, err
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return store.Message{}, err
	}
	return m.send(ctx, cli, accountID, chat, &waE2E.Message{Conversation: proto.String(text)}, "")
}

// SendMedia sends the file at path to chatJID: as an image for image/* MIME types, otherwise as a
// document. The file is streamed into the media cache and uploaded from there, never read whole
// into memory; the stored message points at the cached copy.
func (m *Manager) SendMedia(ctx context.Context, accountID, chatJID, path, caption string) (store.Message, error) {
	cli, err := m.client(accountID)
	if err != nil {
		return store.Message{}, err
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return store.Message{}, err
	}
	mimeType := cmp.Or(mime.TypeByExtension(filepath.Ext(path)), "application/octet-stream")
	src, err := os.Open(path)
	if err != nil {
		return store.Message{}, err
	}
	cached, err := m.media.Save(accountID, mediaExt(path, mimeType), src)
	src.Close()
	if err != nil {
		return store.Message{}, err
	}
	f, err := os.Open(cached)
	if err != nil {
		return store.Message{}, err
	}
	defer f.Close()
	tmp, err := os.CreateTemp("", "wa-go-upload-*")
	if err != nil {
		return store.Message{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close() // runs before the Remove

	isImage := strings.HasPrefix(mimeType, "image/")
	kind := whatsmeow.MediaDocument
	if isImage {
		kind = whatsmeow.MediaImage
	}
	up, err := cli.UploadReader(ctx, f, tmp, kind)
	if err != nil {
		return store.Message{}, err
	}
	var captionPtr *string
	if caption != "" {
		captionPtr = &caption
	}
	msg := &waE2E.Message{}
	if isImage {
		msg.ImageMessage = &waE2E.ImageMessage{
			Caption: captionPtr, Mimetype: &mimeType,
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
		}
	} else {
		msg.DocumentMessage = &waE2E.DocumentMessage{
			Caption: captionPtr, Mimetype: &mimeType, FileName: proto.String(filepath.Base(path)),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
		}
	}
	return m.send(ctx, cli, accountID, chat, msg, cached)
}

// send sends msg and stores it as sent; mediaPath is the cached copy of its attachment, if any.
func (m *Manager) send(ctx context.Context, cli waClient, accountID string, chat types.JID, msg *waE2E.Message, mediaPath string) (store.Message, error) {
	resp, err := cli.SendMessage(ctx, chat, msg)
	if err != nil {
		return store.Message{}, err
	}
	sm, _, err := toStoreMessage(accountID, types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: types.NewJID(accountID, types.DefaultUserServer), IsFromMe: true},
		ID:            resp.ID,
		Timestamp:     resp.Timestamp,
	}, msg)
	if err == nil {
		sm.MediaPath = mediaPath
		err = m.persist(sm)
	}
	if err != nil {
		return sm, fmt.Errorf("whatsapp: message %s sent but not stored: %w", resp.ID, err)
	}
	return sm, nil
}

// SetPresenceAll sends presence p for every account; PresenceUnavailable when hiding to the tray,
// so no number shows as online. Every account is tried; the failures are joined.
func (m *Manager) SetPresenceAll(ctx context.Context, p types.Presence) error {
	m.mu.RLock()
	clients := maps.Clone(m.accounts)
	m.mu.RUnlock()
	var errs []error
	for id, cli := range clients {
		if err := cli.SendPresence(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("whatsapp: presence of %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// SetPrivacySetting changes one privacy setting of accountID and returns the resulting settings.
func (m *Manager) SetPrivacySetting(ctx context.Context, accountID string, name types.PrivacySettingType, value types.PrivacySetting) (types.PrivacySettings, error) {
	cli, err := m.client(accountID)
	if err != nil {
		return types.PrivacySettings{}, err
	}
	return cli.SetPrivacySetting(ctx, name, value)
}

// MarkRead sends read receipts for the incoming messages among msgs (as read from the store) and
// marks them read locally.
func (m *Manager) MarkRead(ctx context.Context, msgs []store.Message) error {
	type key struct{ account, chat, sender string } // one receipt covers one sender's messages
	groups := map[key][]string{}
	for _, msg := range msgs {
		if !msg.FromMe {
			k := key{msg.AccountID, msg.ChatJID, msg.SenderJID}
			groups[k] = append(groups[k], msg.ID)
		}
	}
	for k, ids := range groups {
		cli, err := m.client(k.account)
		if err != nil {
			return err
		}
		chat, err := types.ParseJID(k.chat)
		if err != nil {
			return err
		}
		sender, err := types.ParseJID(k.sender)
		if err != nil {
			return err
		}
		if err := cli.MarkRead(ctx, ids, time.Now(), chat, sender); err != nil {
			return err
		}
		if err := m.store.UpdateMessageStatus(k.account, k.chat, ids, store.StatusRead); err != nil {
			return err
		}
	}
	return nil
}

// Logout unlinks accountID from its phone, deletes its session and unregisters it.
// Its stored messages are kept.
func (m *Manager) Logout(ctx context.Context, accountID string) error {
	cli, err := m.client(accountID)
	if err != nil {
		return err
	}
	if err := cli.Logout(ctx); err != nil {
		return err
	}
	m.remove(accountID, cli)
	m.emit(ConnectionEvent{AccountID: accountID, State: StateLoggedOut})
	return nil
}

// DownloadMedia fetches the attachment of a stored media message into the cache, records the cached
// path on the message and returns it.
func (m *Manager) DownloadMedia(ctx context.Context, msg store.Message) (string, error) {
	cli, err := m.client(msg.AccountID)
	if err != nil {
		return "", err
	}
	var dl interface {
		proto.Message
		whatsmeow.DownloadableMessage
		GetMimetype() string
	}
	switch msg.Type {
	case TypeImage:
		dl = &waE2E.ImageMessage{}
	case TypeVideo:
		dl = &waE2E.VideoMessage{}
	case TypeAudio:
		dl = &waE2E.AudioMessage{}
	case TypeDocument:
		dl = &waE2E.DocumentMessage{}
	case TypeSticker:
		dl = &waE2E.StickerMessage{}
	default:
		return "", fmt.Errorf("whatsapp: message %s has no media", msg.ID)
	}
	if err := proto.Unmarshal(msg.MediaMeta, dl); err != nil {
		return "", fmt.Errorf("whatsapp: media keys of %s: %w", msg.ID, err)
	}
	tmp, err := os.CreateTemp("", "wa-go-download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close() // runs before the Remove
	// shortcut: downloads to a temp file then copies into the cache; adopt the temp file directly if disk I/O matters
	if err := cli.DownloadToFile(ctx, dl, tmp); err != nil {
		return "", err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	name := ""
	if d, ok := dl.(*waE2E.DocumentMessage); ok {
		name = d.GetFileName()
	}
	path, err := m.media.Save(msg.AccountID, mediaExt(name, dl.GetMimetype()), tmp)
	if err != nil {
		return "", err
	}
	return path, m.store.SetMediaPath(msg.AccountID, msg.ChatJID, msg.ID, path)
}

// Close disconnects every account and closes the session database.
func (m *Manager) Close() error {
	m.mu.RLock()
	clients := slices.Collect(maps.Values(m.accounts))
	m.mu.RUnlock()
	for _, cli := range clients {
		cli.Disconnect()
	}
	return m.container.Close()
}

var extRe = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`) // what media.Cache.Save accepts

// mediaExt picks a cache extension: the file name's, else the MIME subtype's, else ".bin".
func mediaExt(fileName, mimeType string) string {
	if ext := filepath.Ext(fileName); extRe.MatchString(ext) {
		return ext
	}
	typ, _, _ := strings.Cut(mimeType, ";")
	_, sub, _ := strings.Cut(typ, "/")
	if ext := "." + strings.TrimSpace(sub); extRe.MatchString(ext) {
		return ext
	}
	return ".bin"
}
