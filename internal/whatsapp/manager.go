package whatsapp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waStore "go.mau.fi/whatsmeow/store"
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
	// ReceiptEvent: the stored status of IDs in ChatJID moved forward to Status. FromMe: the receipt
	// came from one of the user's own devices (incoming messages read there: unread counts dropped).
	ReceiptEvent struct {
		AccountID, ChatJID string
		IDs                []string
		Status             string
		FromMe             bool
	}
	// ChatReadEvent: ChatJID was marked read on another of the user's devices; its unread count dropped.
	ChatReadEvent struct{ AccountID, ChatJID string }
	// ChatNamedEvent: ChatJID (a group) got or changed its name.
	ChatNamedEvent struct{ AccountID, ChatJID, Name string }
	// PushNameEvent: the account's own name is known now, which presence updates need (a fresh pairing
	// learns it only after connecting): send the presence again.
	PushNameEvent struct{ AccountID string }
	// RevisedEvent: stored message ID in ChatJID was edited or deleted for everyone; reload it.
	RevisedEvent struct{ AccountID, ChatJID, ID string }
	// HistorySyncEvent: a history-sync blob was stored; reload the account's chat list.
	HistorySyncEvent struct{ AccountID string }
	// ConnectionEvent: the account's connection state changed (State* constants).
	ConnectionEvent struct{ AccountID, State, Detail string }
	// PeopleEvent: contact or group member information changed; reload the account's people directory.
	PeopleEvent struct{ AccountID string }
	// AvatarEvent: JID's profile picture changed or was cleared.
	AvatarEvent struct{ AccountID, JID string }
	// ErrorEvent: an incoming event could not be fully handled (for example a store write failed).
	ErrorEvent struct {
		AccountID string
		Err       error
	}
)

// Picture is a WhatsApp profile or group picture location.
type Picture struct {
	ID  string
	URL string
}

// ErrNoPicture is returned when a contact or group has no profile picture or it is hidden.
var ErrNoPicture = errors.New("whatsapp: no profile picture")

// Member is a participant in a WhatsApp group.
type Member struct {
	JID   string
	Admin bool
}

// GroupDetails describes a WhatsApp group's metadata and participants.
type GroupDetails struct {
	Subject     string
	Description string
	Members     []Member
}

// msgStore is the part of *store.DB the manager writes to.
type msgStore interface {
	InsertMessagesBatch([]store.Message) error
	InsertMessage(store.Message) (bool, error)
	UpsertChat(accountID, jid, name string, lastMessageTS int64) error
	NameChatIfUnnamed(accountID, jid, name string) error
	ChatName(accountID, jid string) (string, error)
	ChatExpiration(accountID, chatJID string) (int64, error)
	UpdateMessageStatus(accountID, chatJID string, ids []string, status string) error
	MarkChatReadUntil(accountID, chatJID string, until int64) error
	ReviseMessage(accountID, chatJID, id, msgType, body string) (bool, error)
	MessageSender(accountID, chatJID, id string) (string, error)
	SetMediaPath(accountID, chatJID, id, path string) error
	UpsertPeople(accountID string, people []store.Person) error
	SetAvatar(accountID, jid, avatarID string, ts int64) error
}

// Manager runs every paired account. It is safe for concurrent use and never touches the UI.
// Outbound calls block on the network, so call them off the UI thread.
type Manager struct {
	store     msgStore
	media     *media.Cache
	container *sqlstore.Container

	mu        sync.RWMutex // guards accounts, listeners, proxy and groupsAsked; never held while calling whatsmeow or a listener
	accounts  map[string]waClient
	listeners []func(evt any)
	proxy     string // every client's proxy, "" for a direct connection

	connect      func(*whatsmeow.Client) error // (*whatsmeow.Client).Connect; tests fake the handshake
	groupsAsked  map[string]bool               // "account chat" of unnamed groups whose name was asked for this run
	groupSlots   chan struct{}                 // bounds the group-name lookups under way
	groupFetches sync.WaitGroup                // the lookups under way

	peopleMu sync.RWMutex
	people   map[string]store.Person // key: accountID + " " + jid
	members  map[string][]string     // group participants, key: accountID + " " + group jid; guarded by peopleMu
}

// New opens (creating if needed) the whatsmeow session database at sessionPath, which holds every
// paired device. Call Start to connect them.
func New(ctx context.Context, sessionPath string, db *store.DB, cache *media.Cache) (*Manager, error) {
	waStore.DeviceProps.Os = proto.String("Wisp") // the name the phone lists under Linked devices
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)") // sqlstore refuses to run without it
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	container, err := sqlstore.New(ctx, "sqlite", "file:"+sessionPath+"?"+q.Encode(), nil) // nil: no-op logger
	if err != nil {
		return nil, fmt.Errorf("whatsapp: open sessions: %w", err)
	}
	return &Manager{store: db, media: cache, container: container, accounts: map[string]waClient{},
		connect: (*whatsmeow.Client).Connect, groupsAsked: map[string]bool{}, groupSlots: make(chan struct{}, 2),
		people: map[string]store.Person{}, members: map[string][]string{}}, nil
}

// Start registers a client for every saved device and returns: Accounts lists them at once, while
// each connects in the background (a ConnectionEvent reports it; a failed first attempt keeps
// retrying, and a connection error is sent as an ErrorEvent). Add listeners before calling it.
func (m *Manager) Start(ctx context.Context) error {
	devices, err := m.container.GetAllDevices(ctx)
	if err != nil {
		return fmt.Errorf("whatsapp: load sessions: %w", err)
	}
	for _, dev := range devices {
		cli := m.newClient(dev)
		cli.InitialAutoReconnect = true // app started offline: keep retrying in the background
		id := dev.ID.User
		m.add(id, liveClient{cli})
		go func() {
			if err := m.connect(cli); err != nil {
				m.emit(ErrorEvent{AccountID: id, Err: fmt.Errorf("whatsapp: connect %s: %w", id, err)})
			}
		}()
	}
	return nil
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

// mentionAll matches the @all / @everyone word that mentions every participant of a group.
var mentionAll = regexp.MustCompile(`(?i)(?:^|\s)@(?:all|everyone)\b`)

// SendText sends text to chatJID from accountID and stores it as sent. reply, when not nil, is the
// stored message it answers. mentions are the JIDs text mentions (as @<user part>); in a group,
// @all or @everyone adds every participant.
func (m *Manager) SendText(ctx context.Context, accountID, chatJID, text string, reply *store.Message, mentions []string) (store.Message, error) {
	cli, err := m.client(accountID)
	if err != nil {
		return store.Message{}, err
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return store.Message{}, err
	}
	if chat.Server == types.GroupServer && mentionAll.MatchString(text) {
		all, err := m.GroupParticipants(ctx, accountID, chatJID)
		if err != nil {
			return store.Message{}, err
		}
		mentions = append(slices.Clone(mentions), all...)
	}
	msg := &waE2E.Message{Conversation: proto.String(text)}
	if len(mentions) > 0 {
		seen := map[string]bool{}
		ci := contextOf(msg)
		for _, j := range mentions {
			if j != "" && !seen[j] {
				seen[j] = true
				ci.MentionedJID = append(ci.MentionedJID, j)
			}
		}
	}
	quote(msg, reply)
	return m.send(ctx, cli, accountID, chat, msg, "")
}

// GroupParticipants returns the JIDs of the group's participants, from memory once the group was
// looked up (GroupDetails) and until its membership changes.
func (m *Manager) GroupParticipants(ctx context.Context, accountID, groupJID string) ([]string, error) {
	m.peopleMu.RLock()
	cached, ok := m.members[accountID+" "+groupJID]
	m.peopleMu.RUnlock()
	if ok {
		return cached, nil
	}
	d, err := m.GroupDetails(ctx, accountID, groupJID)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(d.Members))
	for i, mem := range d.Members {
		out[i] = mem.JID
	}
	return out, nil
}

// SendMedia sends the file at path to chatJID: as an image for JPEG and PNG (what WhatsApp shows
// inline), otherwise as a document. The file is streamed into the media cache and uploaded from there, never read whole
// into memory; the stored message points at the cached copy. reply is as in SendText.
func (m *Manager) SendMedia(ctx context.Context, accountID, chatJID, path, caption string, reply *store.Message) (store.Message, error) {
	cli, err := m.client(accountID)
	if err != nil {
		return store.Message{}, err
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return store.Message{}, err
	}
	mimeType := cmp.Or(mime.TypeByExtension(filepath.Ext(path)), "application/octet-stream")
	isImage := mimeType == "image/jpeg" || mimeType == "image/png"
	kind := whatsmeow.MediaDocument
	if isImage {
		kind = whatsmeow.MediaImage
	}
	up, cached, err := m.upload(ctx, cli, accountID, path, mediaExt(path, mimeType), kind)
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
	quote(msg, reply)
	return m.send(ctx, cli, accountID, chat, msg, cached)
}

// SendGIF sends the MP4 at mp4Path to chatJID as a GIF: a video the recipient's app loops silently.
// Sets JPEGThumbnail, Width, Height when preview != nil.
// Like SendMedia it streams the file into the media cache and uploads the cached copy.
func (m *Manager) SendGIF(ctx context.Context, accountID, chatJID, mp4Path string, preview []byte, w, h int) (store.Message, error) {
	cli, err := m.client(accountID)
	if err != nil {
		return store.Message{}, err
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return store.Message{}, err
	}
	up, cached, err := m.upload(ctx, cli, accountID, mp4Path, ".mp4", whatsmeow.MediaVideo)
	if err != nil {
		return store.Message{}, err
	}
	vm := &waE2E.VideoMessage{
		GifPlayback: proto.Bool(true), Mimetype: proto.String("video/mp4"),
		URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
		FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
	}
	if preview != nil {
		vm.JPEGThumbnail = preview
		vm.Width = proto.Uint32(uint32(w))
		vm.Height = proto.Uint32(uint32(h))
	}
	msg := &waE2E.Message{VideoMessage: vm}
	return m.send(ctx, cli, accountID, chat, msg, cached)
}

// ProfilePicture queries the profile picture of jid. It reports ok false with nil error
// when knownID is still current (unchanged). It returns ErrNoPicture when no picture is set
// or it is hidden by privacy settings.
func (m *Manager) ProfilePicture(ctx context.Context, accountID, jid, knownID string, full bool) (Picture, bool, error) {
	cli, err := m.client(accountID)
	if err != nil {
		return Picture{}, false, err
	}
	target, err := types.ParseJID(jid)
	if err != nil {
		return Picture{}, false, err
	}
	info, err := cli.GetProfilePictureInfo(ctx, target, &whatsmeow.GetProfilePictureParams{
		Preview:    !full,
		ExistingID: knownID,
	})
	if err != nil {
		if errors.Is(err, whatsmeow.ErrProfilePictureNotSet) || errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized) {
			return Picture{}, false, ErrNoPicture
		}
		return Picture{}, false, fmt.Errorf("whatsapp: profile picture: %w", err)
	}
	if info == nil {
		return Picture{}, false, nil
	}
	return Picture{ID: info.ID, URL: info.URL}, true, nil
}

// FetchPicture downloads picture bytes through the configured proxy, capped at 2 MB,
// with a 20-second timeout.
func (m *Manager) FetchPicture(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	tr := &http.Transport{}
	if proxyAddr := m.proxyAddress(); proxyAddr != "" {
		pu, err := url.Parse(proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("whatsapp: fetch picture: parse proxy: %w", err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	client := &http.Client{Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: fetch picture: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: fetch picture: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("whatsapp: fetch picture: HTTP %d", resp.StatusCode)
	}
	const maxBytes = 2 << 20 // 2 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("whatsapp: fetch picture: %w", err)
	}
	if len(data) > maxBytes {
		return nil, errors.New("whatsapp: picture exceeds 2 MB")
	}
	return data, nil
}

// About queries the status text of jid.
func (m *Manager) About(ctx context.Context, accountID, jid string) (string, error) {
	cli, err := m.client(accountID)
	if err != nil {
		return "", err
	}
	target, err := types.ParseJID(jid)
	if err != nil {
		return "", err
	}
	res, err := cli.GetUserInfo(ctx, []types.JID{target})
	if err != nil {
		return "", fmt.Errorf("whatsapp: user info: %w", err)
	}
	if info, ok := res[target]; ok {
		return info.Status, nil
	}
	if info, ok := res[target.ToNonAD()]; ok {
		return info.Status, nil
	}
	return "", nil
}

// GroupDetails queries metadata and participants of group jid, and upserts group members into the people store.
func (m *Manager) GroupDetails(ctx context.Context, accountID, jid string) (GroupDetails, error) {
	cli, err := m.client(accountID)
	if err != nil {
		return GroupDetails{}, err
	}
	group, err := types.ParseJID(jid)
	if err != nil {
		return GroupDetails{}, err
	}
	info, err := cli.GetGroupInfo(ctx, group)
	if err != nil {
		return GroupDetails{}, fmt.Errorf("whatsapp: group info: %w", err)
	}
	members := make([]Member, len(info.Participants))
	people := make([]store.Person, len(info.Participants))
	for i, p := range info.Participants {
		mJID := p.JID.ToNonAD().String()
		members[i] = Member{
			JID:   mJID,
			Admin: p.IsAdmin || p.IsSuperAdmin,
		}
		var pnJID string
		if p.PhoneNumber.Server == types.DefaultUserServer && p.PhoneNumber.User != "" {
			pnJID = p.PhoneNumber.ToNonAD().String()
		} else if p.JID.Server == types.DefaultUserServer {
			pnJID = p.JID.ToNonAD().String()
		}
		people[i] = store.Person{
			JID:   mJID,
			PNJID: pnJID,
		}
	}
	if len(people) > 0 {
		if err := m.upsertPeople(accountID, people); err != nil {
			return GroupDetails{}, err
		}
	}
	jids := make([]string, len(members))
	for i, mem := range members {
		jids[i] = mem.JID
	}
	m.peopleMu.Lock()
	m.members[accountID+" "+group.String()] = jids
	m.peopleMu.Unlock()
	return GroupDetails{
		Subject:     info.Name,
		Description: info.Topic,
		Members:     members,
	}, nil
}

// upsertPeople inserts or updates people in the store and emits PeopleEvent if anything changed.
func (m *Manager) upsertPeople(accountID string, people []store.Person) error {
	if len(people) == 0 {
		return nil
	}
	m.peopleMu.RLock()
	var changed []store.Person
	updated := make(map[string]store.Person)
	for _, p := range people {
		key := accountID + " " + p.JID
		cur, exists := m.people[key]
		isChanged := false
		if !exists {
			isChanged = true
			cur.JID = p.JID
		}
		if p.PNJID != "" && p.PNJID != cur.PNJID {
			cur.PNJID = p.PNJID
			isChanged = true
		}
		if p.FullName != "" && p.FullName != cur.FullName {
			cur.FullName = p.FullName
			isChanged = true
		}
		if p.PushName != "" && p.PushName != cur.PushName {
			cur.PushName = p.PushName
			isChanged = true
		}
		if isChanged {
			updated[key] = cur
			changed = append(changed, p)
		}
	}
	m.peopleMu.RUnlock()

	if len(changed) == 0 {
		return nil
	}

	if err := m.store.UpsertPeople(accountID, changed); err != nil {
		return err
	}

	m.peopleMu.Lock()
	if m.people == nil {
		m.people = make(map[string]store.Person)
	}
	for k, v := range updated {
		cur := m.people[k]
		if cur.JID == "" {
			cur.JID = v.JID
		}
		if v.PNJID != "" {
			cur.PNJID = v.PNJID
		}
		if v.FullName != "" {
			cur.FullName = v.FullName
		}
		if v.PushName != "" {
			cur.PushName = v.PushName
		}
		m.people[k] = cur
	}
	m.peopleMu.Unlock()

	m.emit(PeopleEvent{AccountID: accountID})
	return nil
}

// upload copies the file at path into the media cache (as ext) and uploads the cached copy as kind,
// streaming through a temp file. It returns the upload and the cached path.
func (m *Manager) upload(ctx context.Context, cli waClient, accountID, path, ext string, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, string, error) {
	src, err := os.Open(path)
	if err != nil {
		return whatsmeow.UploadResponse{}, "", err
	}
	cached, err := m.media.Save(accountID, ext, src)
	src.Close()
	if err != nil {
		return whatsmeow.UploadResponse{}, "", err
	}
	f, err := os.Open(cached)
	if err != nil {
		return whatsmeow.UploadResponse{}, "", err
	}
	defer f.Close()
	tmp, err := os.CreateTemp("", "wisp-upload-*")
	if err != nil {
		return whatsmeow.UploadResponse{}, "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close() // runs before the Remove
	up, err := cli.UploadReader(ctx, f, tmp, kind)
	return up, cached, err
}

// SetTyping shows accountID as typing in chatJID (composing) or no longer typing (paused).
func (m *Manager) SetTyping(ctx context.Context, accountID, chatJID string, composing bool) error {
	cli, err := m.client(accountID)
	if err != nil {
		return err
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	state := types.ChatPresencePaused
	if composing {
		state = types.ChatPresenceComposing
	}
	return cli.SendChatPresence(ctx, chat, state, types.ChatPresenceMediaText)
}

// send sends msg and stores it as sent; mediaPath is the cached copy of its attachment, if any. In a
// chat with disappearing messages on, msg disappears too, here and for everyone.
func (m *Manager) send(ctx context.Context, cli waClient, accountID string, chat types.JID, msg *waE2E.Message, mediaPath string) (store.Message, error) {
	exp, err := m.store.ChatExpiration(accountID, chat.String())
	if err != nil {
		return store.Message{}, err
	}
	if exp > 0 {
		disappearing(msg, uint32(min(exp, math.MaxUint32)))
	}
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
		err = m.creditQuote(ctx, cli, &sm)
	}
	if err == nil {
		sm.MediaPath = mediaPath
		err = m.persist(sm)
	}
	if err != nil {
		return sm, fmt.Errorf("whatsapp: message %s sent but not stored: %w", resp.ID, err)
	}
	return sm, nil
}

// Contact is an entry of an account's whatsmeow contact store.
type Contact struct {
	JID  string // phone-number JID
	Name string // best known name: address book, then first name, business name, push name; may be ""
}

// Contacts returns accountID's contacts sorted by name. LID entries are left out: chats are keyed by
// phone-number JIDs, and whatsmeow keeps an LID twin of many contacts.
func (m *Manager) Contacts(ctx context.Context, accountID string) ([]Contact, error) {
	cli, err := m.client(accountID)
	if err != nil {
		return nil, err
	}
	all, err := cli.AllContacts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Contact, 0, len(all))
	for jid, c := range all {
		if jid.Server == types.DefaultUserServer {
			out = append(out, Contact{JID: jid.String(), Name: cmp.Or(c.FullName, c.FirstName, c.BusinessName, c.PushName)})
		}
	}
	slices.SortFunc(out, func(a, b Contact) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.JID, b.JID)) })
	return out, nil
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

// PrivacySettings returns accountID's privacy settings as whatsmeow last saw them (it fetches them
// from WhatsApp the first time, and phone-side changes update its copy).
func (m *Manager) PrivacySettings(ctx context.Context, accountID string) (types.PrivacySettings, error) {
	cli, err := m.client(accountID)
	if err != nil {
		return types.PrivacySettings{}, err
	}
	ps, err := cli.TryFetchPrivacySettings(ctx, false)
	if err != nil {
		return types.PrivacySettings{}, err
	}
	return *ps, nil
}

// SetDefaultDisappearingTimer sets the message timer of accountID's new chats (0 turns it off).
func (m *Manager) SetDefaultDisappearingTimer(ctx context.Context, accountID string, d time.Duration) error {
	cli, err := m.client(accountID)
	if err != nil {
		return err
	}
	return cli.SetDefaultDisappearingTimer(ctx, d)
}

// ErrBadProxy is returned for a proxy address ValidateProxy refuses.
var ErrBadProxy = errors.New("whatsapp: proxy must be socks5://, http:// or https:// host:port")

// ValidateProxy accepts "" (a direct connection) or a socks5, http or https URL of a host and port,
// optionally with user info and a trailing slash. Its error never repeats addr, which may hold a password.
func ValidateProxy(addr string) error {
	if addr == "" {
		return nil
	}
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "socks5" && u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return ErrBadProxy
	}
	if port, err := strconv.Atoi(u.Port()); err != nil || port < 1 || port > 65535 {
		return ErrBadProxy
	}
	return nil
}

// SetProxy routes every account through the proxy at addr ("" for a direct connection; see
// ValidateProxy), accounts added later included. A connected account uses it from its next connection.
func (m *Manager) SetProxy(addr string) error {
	if err := ValidateProxy(addr); err != nil {
		return err
	}
	m.mu.Lock()
	m.proxy = addr
	clients := maps.Clone(m.accounts)
	m.mu.Unlock()
	var errs []error
	for id, cli := range clients {
		if err := cli.SetProxyAddress(addr); err != nil {
			errs = append(errs, fmt.Errorf("whatsapp: proxy of %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) proxyAddress() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.proxy
}

// MarkRead sends read receipts for the incoming messages among msgs (as read from the store) and
// marks them read locally. Receipts are addressed as the messages arrived (LID or phone number),
// falling back to the stored key where that is unknown. selfOnly sends read-self receipts, which
// only the user's own devices see (read receipts off): the senders learn nothing.
func (m *Manager) MarkRead(ctx context.Context, msgs []store.Message, selfOnly bool) error {
	typ := types.ReceiptTypeRead
	if selfOnly {
		typ = types.ReceiptTypeReadSelf
	}
	type key struct{ account, chat, wireChat, wireSender string } // one receipt covers one sender's messages
	groups := map[key][]string{}
	for _, msg := range msgs {
		if !msg.FromMe {
			k := key{msg.AccountID, msg.ChatJID, cmp.Or(msg.WireChatJID, msg.ChatJID), cmp.Or(msg.WireSenderJID, msg.SenderJID)}
			groups[k] = append(groups[k], msg.ID)
		}
	}
	for k, ids := range groups {
		cli, err := m.client(k.account)
		if err != nil {
			return err
		}
		chat, err := types.ParseJID(k.wireChat)
		if err != nil {
			return err
		}
		sender, err := types.ParseJID(k.wireSender)
		if err != nil {
			return err
		}
		if err := cli.MarkRead(ctx, ids, time.Now(), chat, sender, typ); err != nil {
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
	tmp, err := os.CreateTemp("", "wisp-download-*")
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
	// best effort: the download stands without it
	if err := media.MarkFromInternet(path); err != nil {
		log.Printf("whatsapp: mark of the web on %s: %v", filepath.Base(path), err)
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
