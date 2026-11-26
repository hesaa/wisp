package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/media"
	"wa-go/internal/store"
)

const (
	acctA = "6281111111111"
	acctB = "6282222222222"
)

var (
	ctx   = context.Background()
	alice = types.NewJID("6283333333333", types.DefaultUserServer)
)

// fakeClient records what the manager asks of an account; methods it does not fake panic (nil embed).
type fakeClient struct {
	waClient
	presences   []types.Presence
	presenceErr error
	sent        []*waE2E.Message
	uploads     []whatsmeow.MediaType
	chatStates  []chatState // one per SendChatPresence call
	download    []byte
	receipts    []markRead              // one per MarkRead call
	pns         map[types.JID]types.JID // LID -> phone number
	names       map[types.JID]string    // address-book names
	contacts    map[types.JID]types.ContactInfo
	privacy     types.PrivacySettings
	privacyErr  error
	fetches     []bool // ignoreCache of each TryFetchPrivacySettings call
	timers      []time.Duration
	proxies     []string
	groupNames  map[types.JID]string // GetGroupInfo answers
	groups      map[types.JID]*types.GroupInfo
	groupMu     sync.Mutex
	groupN      int // GetGroupInfo calls
	picInfo     map[types.JID]*types.ProfilePictureInfo
	picErr      map[types.JID]error
	userInfo    map[types.JID]types.UserInfo
	userInfoErr error
	isOnWAMu    sync.Mutex
	isOnWAResp  map[string]types.IsOnWhatsAppResponse
	isOnWAErr   error
	isOnWACalls [][]string
}

func (f *fakeClient) GetGroupInfo(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
	f.groupMu.Lock()
	f.groupN++
	f.groupMu.Unlock()
	if f.groups != nil {
		if g, ok := f.groups[jid]; ok {
			return g, nil
		}
	}
	return &types.GroupInfo{JID: jid, GroupName: types.GroupName{Name: f.groupNames[jid]}}, nil
}

func (f *fakeClient) GetProfilePictureInfo(_ context.Context, jid types.JID, params *whatsmeow.GetProfilePictureParams) (*types.ProfilePictureInfo, error) {
	if f.picErr != nil {
		if err, ok := f.picErr[jid]; ok {
			return nil, err
		}
	}
	if f.picInfo != nil {
		if p, ok := f.picInfo[jid]; ok {
			if params != nil && params.ExistingID != "" && p != nil && params.ExistingID == p.ID {
				return nil, nil
			}
			return p, nil
		}
	}
	return nil, whatsmeow.ErrProfilePictureNotSet
}

func (f *fakeClient) GetUserInfo(_ context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error) {
	if f.userInfoErr != nil {
		return nil, f.userInfoErr
	}
	out := make(map[types.JID]types.UserInfo)
	for _, j := range jids {
		if f.userInfo != nil {
			if u, ok := f.userInfo[j]; ok {
				out[j] = u
			}
		}
	}
	return out, nil
}

func (f *fakeClient) IsOnWhatsApp(_ context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
	f.isOnWAMu.Lock()
	f.isOnWACalls = append(f.isOnWACalls, slices.Clone(phones))
	err := f.isOnWAErr
	respMap := f.isOnWAResp
	f.isOnWAMu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]types.IsOnWhatsAppResponse, 0, len(phones))
	for _, p := range phones {
		if r, ok := respMap[p]; ok {
			out = append(out, r)
		} else {
			out = append(out, types.IsOnWhatsAppResponse{Query: p, IsIn: false})
		}
	}
	return out, nil
}

func (f *fakeClient) groupCalls() int {
	f.groupMu.Lock()
	defer f.groupMu.Unlock()
	return f.groupN
}

func (f *fakeClient) TryFetchPrivacySettings(_ context.Context, ignoreCache bool) (*types.PrivacySettings, error) {
	f.fetches = append(f.fetches, ignoreCache)
	if f.privacyErr != nil {
		return nil, f.privacyErr
	}
	ps := f.privacy
	return &ps, nil
}

func (f *fakeClient) SetDefaultDisappearingTimer(_ context.Context, d time.Duration) error {
	f.timers = append(f.timers, d)
	return nil
}

func (f *fakeClient) SetProxyAddress(addr string, _ ...whatsmeow.SetProxyOptions) error {
	f.proxies = append(f.proxies, addr)
	return nil
}

func (f *fakeClient) AllContacts(context.Context) (map[types.JID]types.ContactInfo, error) {
	return f.contacts, nil
}

type chatState struct {
	chat  types.JID
	state types.ChatPresence
	media types.ChatPresenceMedia
}

func (f *fakeClient) SendChatPresence(_ context.Context, chat types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error {
	f.chatStates = append(f.chatStates, chatState{chat, state, media})
	return nil
}

type markRead struct {
	chat, sender types.JID
	ids          []string
	typ          types.ReceiptType
}

func (f *fakeClient) Disconnect() {}

func (f *fakeClient) PNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	return f.pns[lid], nil
}

func (f *fakeClient) ContactName(_ context.Context, jid types.JID) (string, error) {
	return f.names[jid], nil
}

func (f *fakeClient) SendPresence(_ context.Context, p types.Presence) error {
	f.presences = append(f.presences, p)
	return f.presenceErr
}

func (f *fakeClient) SendMessage(_ context.Context, _ types.JID, msg *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.sent = append(f.sent, msg)
	return whatsmeow.SendResponse{ID: fmt.Sprintf("out%d", len(f.sent)), Timestamp: time.Unix(2000, 0)}, nil
}

func (f *fakeClient) MarkRead(_ context.Context, ids []types.MessageID, _ time.Time, chat, sender types.JID, typ ...types.ReceiptType) error {
	r := markRead{chat, sender, ids, types.ReceiptTypeRead}
	if len(typ) == 1 {
		r.typ = typ[0]
	}
	f.receipts = append(f.receipts, r)
	return nil
}

func (f *fakeClient) UploadReader(_ context.Context, r io.Reader, tmp io.ReadWriteSeeker, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.uploads = append(f.uploads, kind)
	n, err := io.Copy(tmp, r)
	return whatsmeow.UploadResponse{DirectPath: "/direct", FileLength: uint64(n)}, err
}

func (f *fakeClient) DownloadToFile(_ context.Context, _ whatsmeow.DownloadableMessage, file whatsmeow.File) error {
	_, err := file.Write(f.download)
	return err
}

// spyStore records the largest InsertMessagesBatch call; with fail set, message and status writes fail.
type spyStore struct {
	*store.DB
	maxBatch int
	fail     error
}

func (s *spyStore) InsertMessagesBatch(msgs []store.Message) error {
	s.maxBatch = max(s.maxBatch, len(msgs))
	if s.fail != nil {
		return s.fail
	}
	return s.DB.InsertMessagesBatch(msgs)
}

func (s *spyStore) InsertMessage(msg store.Message) (bool, error) {
	if s.fail != nil {
		return false, s.fail
	}
	return s.DB.InsertMessage(msg)
}

func (s *spyStore) UpdateMessageStatus(accountID, chatJID string, ids []string, status string) error {
	if s.fail != nil {
		return s.fail
	}
	return s.DB.UpdateMessageStatus(accountID, chatJID, ids, status)
}

func newTestManager(t *testing.T) (*Manager, *spyStore) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wa.db"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(ctx, filepath.Join(dir, "session.db"), db, media.New(filepath.Join(dir, "cache")))
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		m.Close()
		db.Close()
	})
	spy := &spyStore{DB: db}
	m.store = spy
	return m, spy
}

func incoming(chat types.JID, id, text string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: alice},
			ID:            id, PushName: "Alice", Timestamp: time.Unix(1000, 0),
		},
		Message: &waE2E.Message{Conversation: proto.String(text)},
	}
}

func statusOf(t *testing.T, db *spyStore, acct, id string) string {
	t.Helper()
	page, err := db.GetChatMessages(acct, alice.String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range page {
		if m.ID == id {
			return m.Status
		}
	}
	return ""
}

func TestRoutesMessageToItsAccountOnly(t *testing.T) {
	m, db := newTestManager(t)
	fa, fb := &fakeClient{}, &fakeClient{}
	m.add(acctA, fa)
	m.add(acctB, fb)
	if got := m.Accounts(); !slices.Equal(got, []string{acctA, acctB}) {
		t.Fatalf("accounts %v", got)
	}
	var got []MessageEvent
	m.AddListener(func(e any) {
		if me, ok := e.(MessageEvent); ok {
			got = append(got, me)
		}
	})

	for range 2 { // the second is a redelivery
		if err := m.handleEvent(ctx, acctA, fa, incoming(alice, "m1", "hi")); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 1 {
		t.Fatalf("listener got %d events, want 1 (a redelivery must not re-emit)", len(got))
	}
	ev := got[0]
	if ev.AccountID != acctA || ev.Message.AccountID != acctA || ev.Message.Body != "hi" ||
		ev.Message.Status != store.StatusReceived || ev.Message.FromMe {
		t.Fatalf("event %#v", got[0])
	}
	pa, _ := db.GetChatMessages(acctA, alice.String(), 0)
	pb, _ := db.GetChatMessages(acctB, alice.String(), 0)
	ca, _ := db.ListChats(acctA)
	cb, _ := db.ListChats(acctB)
	if len(pa) != 1 || len(pb) != 0 || len(cb) != 0 {
		t.Fatalf("A msgs=%d B msgs=%d B chats=%d", len(pa), len(pb), len(cb))
	}
	if len(ca) != 1 || ca[0].Name != "Alice" || ca[0].Unread != 1 {
		t.Fatalf("A chats %+v", ca)
	}
}

func TestSetPresenceAllReachesEveryAccount(t *testing.T) {
	m, _ := newTestManager(t)
	fa, fb := &fakeClient{}, &fakeClient{presenceErr: errors.New("offline")}
	m.add(acctA, fa)
	m.add(acctB, fb)
	err := m.SetPresenceAll(ctx, types.PresenceUnavailable)
	if err == nil || !strings.Contains(err.Error(), acctB) {
		t.Fatalf("want B's failure reported, got %v", err)
	}
	for _, f := range []*fakeClient{fa, fb} {
		if !slices.Equal(f.presences, []types.Presence{types.PresenceUnavailable}) {
			t.Fatalf("presences %v", f.presences)
		}
	}
}

func TestReceiptsAdvanceStatusPerAccount(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	sent, err := m.SendText(ctx, acctA, alice.String(), "hello", nil, nil)
	if err != nil || sent.Status != store.StatusSent || !sent.FromMe || sent.Body != "hello" ||
		fa.sent[0].GetConversation() != "hello" {
		t.Fatalf("sent %+v err %v", sent, err)
	}
	// the same chat and message id under account B must not move
	if err := db.InsertMessagesBatch([]store.Message{{ID: sent.ID, ChatJID: alice.String(), AccountID: acctB,
		Timestamp: 1, Status: store.StatusSent, FromMe: true}}); err != nil {
		t.Fatal(err)
	}
	receipt := func(typ types.ReceiptType, id string) *events.Receipt {
		return &events.Receipt{MessageSource: types.MessageSource{Chat: alice, Sender: alice}, MessageIDs: []string{id}, Type: typ}
	}
	var got []any
	m.AddListener(func(e any) { got = append(got, e) })

	if err := m.handleEvent(ctx, acctA, fa, receipt(types.ReceiptTypeDelivered, sent.ID)); err != nil {
		t.Fatal(err)
	}
	if s := statusOf(t, db, acctA, sent.ID); s != store.StatusDelivered {
		t.Fatalf("after delivered: %s", s)
	}
	if err := m.handleEvent(ctx, acctA, fa, receipt(types.ReceiptTypeRead, sent.ID)); err != nil {
		t.Fatal(err)
	}
	if s := statusOf(t, db, acctA, sent.ID); s != store.StatusRead {
		t.Fatalf("after read: %s", s)
	}
	if s := statusOf(t, db, acctB, sent.ID); s != store.StatusSent {
		t.Fatalf("B's copy moved to %s", s)
	}
	if ev, ok := got[1].(ReceiptEvent); !ok || ev.AccountID != acctA || ev.Status != store.StatusRead || ev.IDs[0] != sent.ID {
		t.Fatalf("event %#v", got[1])
	}

	// reading an incoming message on another of the user's devices clears it here
	if err := m.handleEvent(ctx, acctA, fa, incoming(alice, "in1", "yo")); err != nil {
		t.Fatal(err)
	}
	if err := m.handleEvent(ctx, acctA, fa, receipt(types.ReceiptTypeReadSelf, "in1")); err != nil {
		t.Fatal(err)
	}
	if chats, _ := db.ListChats(acctA); chats[0].Unread != 0 {
		t.Fatalf("unread %d after read-self", chats[0].Unread)
	}
}

func TestStatusBroadcastStoredWithoutChat(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	if err := m.handleEvent(ctx, acctA, fa, incoming(types.StatusBroadcastJID, "s1", "my story")); err != nil {
		t.Fatal(err)
	}
	page, _ := db.GetChatMessages(acctA, types.StatusBroadcastJID.String(), 0)
	chats, _ := db.ListChats(acctA)
	if len(page) != 1 || page[0].Body != "my story" || len(chats) != 0 {
		t.Fatalf("stored %d, chats %+v", len(page), chats)
	}
}

func TestHistorySyncInsertsInBatches(t *testing.T) {
	m, db := newTestManager(t)
	dev := m.container.NewDevice()
	dev.ID = &types.JID{User: acctA, Server: types.DefaultUserServer}
	cli := liveClient{whatsmeow.NewClient(dev, nil)} // offline: only ParseWebMessage is used

	conv := &waHistorySync.Conversation{ID: proto.String(alice.String()), Name: proto.String("Alice"), UnreadCount: proto.Uint32(2)}
	for i := range 250 {
		conv.Messages = append(conv.Messages, &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String(alice.String()), FromMe: proto.Bool(i%10 == 0), ID: proto.String(fmt.Sprintf("h%03d", i))},
			MessageTimestamp: proto.Uint64(uint64(1000 + i)),
			Message:          &waE2E.Message{Conversation: proto.String("old")},
		}})
	}
	var got []any
	m.AddListener(func(e any) { got = append(got, e) })
	stories := []*waWeb.WebMessageInfo{}
	for i := range 3 {
		stories = append(stories, &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String(types.StatusBroadcastJID.String()), Participant: proto.String(alice.String()), ID: proto.String(fmt.Sprintf("s%d", i))},
			MessageTimestamp: proto.Uint64(uint64(2000 + i)),
			Message:          &waE2E.Message{Conversation: proto.String("story")},
		})
	}
	evt := &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{conv}, StatusV3Messages: stories}}
	if err := m.handleEvent(ctx, acctA, cli, evt); err != nil {
		t.Fatal(err)
	}

	if db.maxBatch == 0 || db.maxBatch > store.BatchSize {
		t.Fatalf("largest InsertMessagesBatch call: %d", db.maxBatch)
	}
	n := 0
	for p := 0; ; p++ {
		page, err := db.GetChatMessages(acctA, alice.String(), p)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		n += len(page)
	}
	if n != 250 {
		t.Fatalf("stored %d of 250", n)
	}
	if st, _ := db.GetChatMessages(acctA, types.StatusBroadcastJID.String(), 0); len(st) != 3 || st[0].SenderJID != alice.String() {
		t.Fatalf("stories stored: %+v", st)
	}
	chats, _ := db.ListChats(acctA) // stories add no chat row
	if len(chats) != 1 || chats[0].Name != "Alice" || chats[0].Unread != 2 {
		t.Fatalf("chats %+v (want name Alice, unread 2)", chats)
	}
	if len(got) != 1 || got[0] != (HistorySyncEvent{AccountID: acctA}) {
		t.Fatalf("events %#v", got)
	}
}

func TestUnknownAccountIsAnError(t *testing.T) {
	m, _ := newTestManager(t)
	m.add(acctA, &fakeClient{})
	ops := map[string]func() error{
		"SendText":  func() error { _, err := m.SendText(ctx, "nope", alice.String(), "x", nil, nil); return err },
		"Logout":    func() error { return m.Logout(ctx, "nope") },
		"SendGIF":   func() error { _, err := m.SendGIF(ctx, "nope", alice.String(), "x.mp4", nil, 0, 0); return err },
		"SetTyping": func() error { return m.SetTyping(ctx, "nope", alice.String(), true) },
		"DownloadMedia": func() error {
			_, err := m.DownloadMedia(ctx, store.Message{AccountID: "nope", Type: TypeImage})
			return err
		},
		"SetPrivacySetting": func() error {
			_, err := m.SetPrivacySetting(ctx, "nope", types.PrivacySettingTypeLastSeen, types.PrivacySettingNone)
			return err
		},
		"PrivacySettings": func() error { _, err := m.PrivacySettings(ctx, "nope"); return err },
		"SetDefaultDisappearingTimer": func() error {
			return m.SetDefaultDisappearingTimer(ctx, "nope", 24*time.Hour)
		},
		"SendTextWithPreview": func() error {
			_, err := m.SendTextWithPreview(ctx, "nope", alice.String(), "x", nil, nil, &LinkPreview{Title: "t"})
			return err
		},
		"CheckPhones": func() error {
			_, err := m.CheckPhones(ctx, "nope", []string{"+6283333333333"})
			return err
		},
	}
	for name, op := range ops {
		if err := op(); !errors.Is(err, ErrUnknownAccount) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestContactsBestNamePhoneJIDsOnly(t *testing.T) {
	m, _ := newTestManager(t)
	bob := types.NewJID("6284444444444", types.DefaultUserServer)
	m.add(acctA, &fakeClient{contacts: map[types.JID]types.ContactInfo{
		alice: {Found: true, FullName: "Alice Smith", FirstName: "Alice", PushName: "ali"},
		bob:   {Found: true, PushName: "Bob"},
		types.NewJID("99887766", types.HiddenUserServer): {Found: true, FullName: "Alice Smith"}, // LID twin
	}})
	got, err := m.Contacts(ctx, acctA)
	if err != nil {
		t.Fatal(err)
	}
	want := []Contact{{JID: alice.String(), Name: "Alice Smith"}, {JID: bob.String(), Name: "Bob"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := m.Contacts(ctx, "nope"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("unknown account: %v", err)
	}
}

func TestIncomingImageDownloadsIntoCache(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{download: []byte("jpeg bytes")}
	m.add(acctA, fa)
	evt := incoming(alice, "img1", "")
	evt.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption: proto.String("look"), Mimetype: proto.String("image/jpeg"),
		DirectPath: proto.String("/x"), MediaKey: []byte("k"), JPEGThumbnail: []byte("thumb"),
	}}
	if err := m.handleEvent(ctx, acctA, fa, evt); err != nil {
		t.Fatal(err)
	}
	page, _ := db.GetChatMessages(acctA, alice.String(), 0)
	if len(page) != 1 || page[0].Type != TypeImage || page[0].Body != "look" || len(page[0].MediaMeta) == 0 {
		t.Fatalf("stored %+v", page)
	}
	path, err := m.DownloadMedia(ctx, page[0])
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "jpeg bytes" || filepath.Ext(path) != ".jpeg" {
		t.Fatalf("cached %s: %q %v", path, b, err)
	}
	page, _ = db.GetChatMessages(acctA, alice.String(), 0)
	if page[0].MediaPath != path {
		t.Fatalf("media path %q, want %q", page[0].MediaPath, path)
	}
}

func TestSendMediaPicksImageOrDocument(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	dir := t.TempDir()
	png := filepath.Join(dir, "photo.png")
	txt := filepath.Join(dir, "notes.txt")
	gif := filepath.Join(dir, "anim.gif")
	if err := errors.Join(os.WriteFile(png, []byte("png bytes"), 0o600), os.WriteFile(txt, []byte("some notes"), 0o600),
		os.WriteFile(gif, []byte("gif bytes"), 0o600)); err != nil {
		t.Fatal(err)
	}

	img, err := m.SendMedia(ctx, acctA, alice.String(), png, "a photo", nil)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := m.SendMedia(ctx, acctA, alice.String(), txt, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fa.sent[0].GetImageMessage().GetCaption() != "a photo" || fa.sent[0].GetImageMessage().GetFileLength() != 9 {
		t.Fatalf("image message %v", fa.sent[0])
	}
	if fa.sent[1].GetDocumentMessage().GetFileName() != "notes.txt" {
		t.Fatalf("document message %v", fa.sent[1])
	}
	if img.Type != TypeImage || doc.Type != TypeDocument || doc.Body != "notes.txt" || img.Status != store.StatusSent {
		t.Fatalf("img %+v doc %+v", img, doc)
	}
	if b, err := os.ReadFile(doc.MediaPath); err != nil || string(b) != "some notes" {
		t.Fatalf("cached copy %q: %q %v", doc.MediaPath, b, err)
	}
	// only JPEG and PNG are images WhatsApp shows; other image types go as documents
	if anim, err := m.SendMedia(ctx, acctA, alice.String(), gif, "", nil); err != nil || anim.Type != TypeDocument ||
		fa.sent[2].GetDocumentMessage().GetMimetype() != "image/gif" || fa.uploads[2] != whatsmeow.MediaDocument {
		t.Fatalf("gif sent as %+v (%v), err %v", anim, fa.sent[2], err)
	}
	if page, _ := db.GetChatMessages(acctA, alice.String(), 0); len(page) != 3 {
		t.Fatalf("stored %d outgoing", len(page))
	}
}

func TestSendGIFIsALoopingVideo(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	mp4 := filepath.Join(t.TempDir(), "giphy-download")
	if err := os.WriteFile(mp4, []byte("mp4 bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := m.SendGIF(ctx, acctA, alice.String(), mp4, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	v := fa.sent[0].GetVideoMessage()
	if !v.GetGifPlayback() || v.GetMimetype() != "video/mp4" || v.GetFileLength() != 9 || v.GetDirectPath() != "/direct" {
		t.Fatalf("video message %v", v)
	}
	if len(fa.uploads) != 1 || fa.uploads[0] != whatsmeow.MediaVideo {
		t.Fatalf("uploaded as %v, want video", fa.uploads)
	}
	if got.Type != TypeVideo || got.Status != store.StatusSent || filepath.Ext(got.MediaPath) != ".mp4" {
		t.Fatalf("stored %+v", got)
	}
	if b, err := os.ReadFile(got.MediaPath); err != nil || string(b) != "mp4 bytes" {
		t.Fatalf("cached copy %q: %q %v", got.MediaPath, b, err)
	}
	if page, _ := db.GetChatMessages(acctA, alice.String(), 0); len(page) != 1 || len(page[0].MediaMeta) == 0 {
		t.Fatalf("stored %+v", page)
	}
}

func TestSetTypingComposingThenPaused(t *testing.T) {
	m, _ := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	if err := errors.Join(m.SetTyping(ctx, acctA, alice.String(), true), m.SetTyping(ctx, acctA, alice.String(), false)); err != nil {
		t.Fatal(err)
	}
	want := []chatState{{alice, types.ChatPresenceComposing, types.ChatPresenceMediaText}, {alice, types.ChatPresencePaused, types.ChatPresenceMediaText}}
	if !slices.Equal(fa.chatStates, want) {
		t.Fatalf("chat states %+v, want %+v", fa.chatStates, want)
	}
	if err := m.SetTyping(ctx, acctA, "1.2.3@s.whatsapp.net", true); err == nil {
		t.Fatal("bad chat JID accepted")
	}
}

func TestMarkReadOneReceiptPerSender(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	group := types.NewJID("120363000000000000", types.GroupServer)
	bob := types.NewJID("6284444444444", types.DefaultUserServer)
	msgs := []store.Message{
		{ID: "a1", ChatJID: group.String(), AccountID: acctA, SenderJID: alice.String(), Timestamp: 1, Status: store.StatusReceived},
		{ID: "a2", ChatJID: group.String(), AccountID: acctA, SenderJID: alice.String(), Timestamp: 2, Status: store.StatusReceived},
		{ID: "b1", ChatJID: group.String(), AccountID: acctA, SenderJID: bob.String(), Timestamp: 3, Status: store.StatusReceived},
		{ID: "me", ChatJID: group.String(), AccountID: acctA, Timestamp: 4, Status: store.StatusSent, FromMe: true},
	}
	if err := db.InsertMessagesBatch(msgs); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkRead(ctx, msgs, false); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(fa.receipts))
	for _, r := range fa.receipts {
		if r.chat != group { // no wire addressing stored: falls back to the stored key
			t.Fatalf("receipt to %s", r.chat)
		}
		got = append(got, strings.Join(r.ids, ","))
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"a1,a2", "b1"}) {
		t.Fatalf("receipts %v", got)
	}
	if chats, _ := db.ListChats(acctA); chats[0].Unread != 0 {
		t.Fatalf("unread %d", chats[0].Unread)
	}
}

func TestFailedStoreWithholdsMessageAck(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	var errs []ErrorEvent
	m.AddListener(func(e any) {
		if ee, ok := e.(ErrorEvent); ok {
			errs = append(errs, ee)
		}
	})
	db.fail = errors.New("disk full")
	if m.onEvent(ctx, acctA, fa, incoming(alice, "m1", "hi")) {
		t.Fatal("message acked although it was not stored")
	}
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: alice, Sender: alice}, MessageIDs: []string{"x"}, Type: types.ReceiptTypeRead}
	if !m.onEvent(ctx, acctA, fa, receipt) {
		t.Fatal("receipt ack withheld")
	}
	if len(errs) != 2 || errs[0].AccountID != acctA || !errors.Is(errs[0].Err, db.fail) {
		t.Fatalf("error events %+v", errs)
	}
	db.fail = nil
	if !m.onEvent(ctx, acctA, fa, incoming(alice, "m1", "hi")) {
		t.Fatal("redelivered message not acked after the store recovered")
	}
}

func TestLIDChatsStoredUnderPhoneNumber(t *testing.T) {
	m, db := newTestManager(t)
	lid := types.NewJID("123456789012345", types.HiddenUserServer)
	fa := &fakeClient{pns: map[types.JID]types.JID{lid: alice}}
	m.add(acctA, fa)
	evt := incoming(lid, "l1", "via lid")
	evt.Info.Sender = types.JID{User: lid.User, Server: types.HiddenUserServer, Device: 3}
	evt.Info.SenderAlt = types.JID{User: alice.User, Server: types.DefaultUserServer, Device: 3}
	fa.pns = nil // this one must come from SenderAlt
	if err := m.handleEvent(ctx, acctA, fa, evt); err != nil {
		t.Fatal(err)
	}
	page, _ := db.GetChatMessages(acctA, alice.String(), 0)
	if len(page) != 1 || page[0].SenderJID != alice.String() {
		t.Fatalf("stored under phone number: %+v", page)
	}
	if err := m.MarkRead(ctx, page, false); err != nil {
		t.Fatal(err)
	}
	if r := fa.receipts; len(r) != 1 || r[0].chat != lid || r[0].sender.String() != lid.String() || statusOf(t, db, acctA, "l1") != store.StatusRead {
		t.Fatalf("read receipt %+v (want it addressed to the LID it came from)", r)
	}
	// a receipt addressed by LID alone is resolved through the account's LID map
	fa.pns = map[types.JID]types.JID{lid: alice}
	sent, err := m.SendText(ctx, acctA, alice.String(), "reply", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []string{sent.ID}, Type: types.ReceiptTypeRead}
	if err := m.handleEvent(ctx, acctA, fa, receipt); err != nil {
		t.Fatal(err)
	}
	if s := statusOf(t, db, acctA, sent.ID); s != store.StatusRead {
		t.Fatalf("reply status %q after LID receipt", s)
	}
	if chats, _ := db.ListChats(acctA); len(chats) != 1 {
		t.Fatalf("one contact became %d chats: %+v", len(chats), chats)
	}
	// with no mapping at all the LID is kept rather than the message dropped
	other := types.NewJID("999999999999999", types.HiddenUserServer)
	if err := m.handleEvent(ctx, acctA, fa, incoming(other, "o1", "unknown")); err != nil {
		t.Fatal(err)
	}
	if page, _ := db.GetChatMessages(acctA, other.String(), 0); len(page) != 1 {
		t.Fatalf("unmapped LID message: %+v", page)
	}
}

func TestEditAndRevokeOnlyByTheAuthor(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	group := types.NewJID("120363000000000000", types.GroupServer)
	bob := types.NewJID("6284444444444", types.DefaultUserServer)
	carol := types.NewJID("6285555555555", types.DefaultUserServer)
	carolLID := types.NewJID("222222222222222", types.HiddenUserServer)
	n := 0
	protocol := func(sender types.JID, pm *waE2E.ProtocolMessage) *events.Message {
		n++
		e := incoming(group, fmt.Sprintf("p%d", n), "")
		e.Info.Sender = sender
		e.Message = &waE2E.Message{ProtocolMessage: pm}
		return e
	}
	edit := func(sender types.JID, target, text string) *events.Message {
		return protocol(sender, &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String(target)},
			EditedMessage: &waE2E.Message{Conversation: proto.String(text)},
		})
	}
	revoke := func(sender types.JID, target string) *events.Message {
		return protocol(sender, &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String(target)},
		})
	}
	handle := func(evts ...*events.Message) {
		t.Helper()
		for _, e := range evts {
			if err := m.handleEvent(ctx, acctA, fa, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows := func() map[string]store.Message {
		page, _ := db.GetChatMessages(acctA, group.String(), 0)
		out := map[string]store.Message{}
		for _, msg := range page {
			out[msg.ID] = msg
		}
		return out
	}

	img := incoming(group, "i1", "")
	img.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("pic"), MediaKey: []byte("k")}}
	fromCarol := incoming(group, "c1", "crol") // carol's LID has no phone number yet
	fromCarol.Info.Sender = types.JID{User: carolLID.User, Server: types.HiddenUserServer, Device: 1}
	handle(incoming(group, "t1", "tpyo"), img, fromCarol)
	own, err := m.SendText(ctx, acctA, group.String(), "mine", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMediaPath(acctA, group.String(), "i1", `C:\cache\pic.jpeg`); err != nil {
		t.Fatal(err)
	}
	before := rows()
	var revised []string
	m.AddListener(func(e any) {
		if ev, ok := e.(RevisedEvent); ok {
			revised = append(revised, ev.ID)
		}
	})

	// forged: another member (or an admin) edits or deletes someone else's message
	untyped := protocol(alice, &waE2E.ProtocolMessage{Key: &waCommon.MessageKey{ID: proto.String("t1")}}) // REVOKE is the zero value
	handle(edit(bob, "t1", "forged"), revoke(bob, "i1"), edit(alice, own.ID, "forged"), revoke(bob, own.ID), untyped)
	if after := rows(); !maps.EqualFunc(before, after, func(a, b store.Message) bool { return a.Body == b.Body && a.Type == b.Type }) || len(revised) != 0 {
		t.Fatalf("forged revisions applied: %+v, events %v", after, revised)
	}

	// the authors themselves: alice; the user from another device (own LID, unmapped); carol, whose
	// stored LID is resolved through the mapping learned since
	ownOtherDevice := edit(types.JID{User: "333333333333333", Server: types.HiddenUserServer, Device: 2}, own.ID, "mine, edited")
	ownOtherDevice.Info.IsFromMe = true
	fa.pns = map[types.JID]types.JID{carolLID: carol}
	handle(edit(alice, "t1", "fixed"), revoke(alice, "i1"), revoke(alice, "i1"), ownOtherDevice, edit(carol, "c1", "carol"))
	r := rows()
	if r["t1"].Body != "fixed" || r[own.ID].Body != "mine, edited" || r["c1"].Body != "carol" {
		t.Fatalf("edits: %+v", r)
	}
	if i := r["i1"]; i.Type != TypeRevoked || i.Body != revokedBody || len(i.MediaMeta) != 0 || i.MediaPath != "" {
		t.Fatalf("revoke: %+v", i)
	}
	if !slices.Equal(revised, []string{"t1", "i1", own.ID, "c1"}) { // the repeated revoke changed nothing
		t.Fatalf("revised events %v", revised)
	}
}

func TestReactionsSkippedUnknownKindsStoredRead(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	react := incoming(alice, "x1", "")
	react.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("ok"), Key: &waCommon.MessageKey{ID: proto.String("m")}}}
	poll := incoming(alice, "p1", "")
	poll.Message = &waE2E.Message{PlaceholderMessage: &waE2E.PlaceholderMessage{}} // a kind still unmapped
	for _, e := range []*events.Message{react, poll} {
		if err := m.handleEvent(ctx, acctA, fa, e); err != nil {
			t.Fatal(err)
		}
	}
	page, _ := db.GetChatMessages(acctA, alice.String(), 0)
	if len(page) != 1 || page[0].ID != "p1" || page[0].Type != TypeUnsupported || page[0].Status != store.StatusRead {
		t.Fatalf("stored %+v", page)
	}
	if chats, _ := db.ListChats(acctA); chats[0].Unread != 0 {
		t.Fatalf("unsupported message counted unread: %d", chats[0].Unread)
	}
}

func TestPushNameNeverReplacesChatName(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	bob := types.NewJID("6284444444444", types.DefaultUserServer)
	if err := db.UpsertChat(acctA, alice.String(), "Alice (work)", 1); err != nil {
		t.Fatal(err)
	}
	dash := incoming(bob, "b1", "hi")
	dash.Info.PushName = "-" // whatsmeow's "no name"
	for _, e := range []*events.Message{incoming(alice, "a1", "hi"), dash} {
		if err := m.handleEvent(ctx, acctA, fa, e); err != nil {
			t.Fatal(err)
		}
	}
	names := func() map[string]string {
		chats, _ := db.ListChats(acctA)
		out := map[string]string{}
		for _, c := range chats {
			out[c.JID] = c.Name
		}
		return out
	}
	if n := names(); n[alice.String()] != "Alice (work)" || n[bob.String()] != "" {
		t.Fatalf("names %v", n)
	}
	fa.names = map[types.JID]string{alice: "Alice Smith"} // the address book wins
	if err := m.handleEvent(ctx, acctA, fa, incoming(alice, "a2", "again")); err != nil {
		t.Fatal(err)
	}
	if n := names(); n[alice.String()] != "Alice Smith" {
		t.Fatalf("names %v", n)
	}
}

func TestPrivacySettingsAndTimerReachOnlyTheirAccount(t *testing.T) {
	m, _ := newTestManager(t)
	fa := &fakeClient{privacy: types.PrivacySettings{LastSeen: types.PrivacySettingContacts}}
	fb := &fakeClient{privacy: types.PrivacySettings{LastSeen: types.PrivacySettingNone, ReadReceipts: types.PrivacySettingNone}}
	m.add(acctA, fa)
	m.add(acctB, fb)
	ps, err := m.PrivacySettings(ctx, acctB)
	if err != nil || ps != fb.privacy {
		t.Fatalf("B's settings %+v %v", ps, err)
	}
	if len(fa.fetches) != 0 || !slices.Equal(fb.fetches, []bool{false}) {
		t.Fatalf("fetches A %v B %v (want B's only, from the cache)", fa.fetches, fb.fetches)
	}
	if err := m.SetDefaultDisappearingTimer(ctx, acctA, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fa.timers, []time.Duration{7 * 24 * time.Hour}) || len(fb.timers) != 0 {
		t.Fatalf("timers A %v B %v", fa.timers, fb.timers)
	}
	fb.privacyErr = errors.New("offline")
	if _, err := m.PrivacySettings(ctx, acctB); !errors.Is(err, fb.privacyErr) {
		t.Fatalf("fetch failure: %v", err)
	}
}

func TestSetProxyValidatesThenReachesEveryAccount(t *testing.T) {
	m, _ := newTestManager(t)
	fa, fb := &fakeClient{}, &fakeClient{}
	m.add(acctA, fa)
	m.add(acctB, fb)
	if err := m.SetProxy("ftp://proxy.lan:21"); !errors.Is(err, ErrBadProxy) {
		t.Fatalf("bad proxy: %v", err)
	}
	if len(fa.proxies)+len(fb.proxies) != 0 {
		t.Fatalf("a rejected proxy reached a client: %v %v", fa.proxies, fb.proxies)
	}
	for _, addr := range []string{"socks5://127.0.0.1:1080", ""} {
		if err := m.SetProxy(addr); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []*fakeClient{fa, fb} {
		if !slices.Equal(f.proxies, []string{"socks5://127.0.0.1:1080", ""}) {
			t.Fatalf("proxies %q", f.proxies)
		}
	}
}

func TestValidateProxy(t *testing.T) {
	for _, ok := range []string{"", "socks5://127.0.0.1:1080", "http://proxy.lan:8080", "https://proxy.example.com:443",
		"socks5://user:secret@10.0.0.1:1080", "http://[::1]:3128", "http://proxy.lan:8080/"} {
		if err := ValidateProxy(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"ftp://proxy.lan:21", "socks4://proxy.lan:1080", "http://proxy.lan", "socks5://:1080",
		"http://proxy.lan:0", "http://proxy.lan:65536", "http://proxy.lan:port", "proxy.lan:8080", "junk", "socks5://",
		"http://proxy.lan:8080/path", "http://proxy.lan:8080?x=1", "http://proxy.lan:8080?", "socks5://h:1080#frag",
		" http://proxy.lan:8080"} {
		if err := ValidateProxy(bad); !errors.Is(err, ErrBadProxy) {
			t.Errorf("%q accepted (%v)", bad, err)
		}
	}
}

func TestMentionsStoredFromText(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)

	// ExtendedTextMessage with MentionedJID
	evtText := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: alice, Sender: alice},
			ID:            "m1",
			Timestamp:     time.Unix(1000, 0),
		},
		Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String("hello @a @b"),
				ContextInfo: &waE2E.ContextInfo{
					MentionedJID: []string{"a@lid", "b@s.whatsapp.net"},
				},
			},
		},
	}
	if err := m.handleEvent(ctx, acctA, fa, evtText); err != nil {
		t.Fatal(err)
	}

	// ImageMessage caption with ContextInfo
	evtImg := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: alice, Sender: alice},
			ID:            "m2",
			Timestamp:     time.Unix(1001, 0),
		},
		Message: &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				Caption: proto.String("photo of @c"),
				ContextInfo: &waE2E.ContextInfo{
					MentionedJID: []string{"c@s.whatsapp.net"},
				},
			},
		},
	}
	if err := m.handleEvent(ctx, acctA, fa, evtImg); err != nil {
		t.Fatal(err)
	}

	msgs, err := db.GetChatMessages(acctA, alice.String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var msg1, msg2 store.Message
	for _, msg := range msgs {
		if msg.ID == "m1" {
			msg1 = msg
		} else if msg.ID == "m2" {
			msg2 = msg
		}
	}
	if msg1.Mentions != "a@lid b@s.whatsapp.net" {
		t.Errorf("m1 Mentions = %q, want %q", msg1.Mentions, "a@lid b@s.whatsapp.net")
	}
	if msg2.Mentions != "c@s.whatsapp.net" {
		t.Errorf("m2 Mentions = %q, want %q", msg2.Mentions, "c@s.whatsapp.net")
	}
}

func TestGroupSenderPushNameStored(t *testing.T) {
	m, db := newTestManager(t)
	lid := types.NewJID("123456789012345", types.HiddenUserServer)
	pn := types.NewJID("628123456789", types.DefaultUserServer)
	group := types.NewJID("1234567-987654", types.GroupServer)
	fa := &fakeClient{groupNames: map[types.JID]string{group: "Test Group"}}
	m.add(acctA, fa)

	var peopleEvents []PeopleEvent
	m.AddListener(func(e any) {
		if pe, ok := e.(PeopleEvent); ok {
			peopleEvents = append(peopleEvents, pe)
		}
	})

	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:      group,
				Sender:    lid,
				SenderAlt: pn,
			},
			ID:        "g1",
			PushName:  "Sinta",
			Timestamp: time.Unix(1000, 0),
		},
		Message: &waE2E.Message{
			Conversation: proto.String("hi all"),
		},
	}

	if err := m.handleEvent(ctx, acctA, fa, evt); err != nil {
		t.Fatal(err)
	}

	people, err := db.People(acctA)
	if err != nil {
		t.Fatal(err)
	}
	var found *store.Person
	for i := range people {
		if people[i].JID == lid.String() {
			found = &people[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("person with JID %s not found in people table; got %+v", lid.String(), people)
	}
	if found.PNJID != pn.String() || found.PushName != "Sinta" {
		t.Errorf("person = %+v, want JID=%s, PNJID=%s, PushName=Sinta", *found, lid.String(), pn.String())
	}
	if len(peopleEvents) != 1 || peopleEvents[0].AccountID != acctA {
		t.Errorf("PeopleEvent count = %d, got events %+v; want exactly 1 for account %s", len(peopleEvents), peopleEvents, acctA)
	}
}

func TestMaskedNameNotStored(t *testing.T) {
	m, db := newTestManager(t)
	contact := types.NewJID("628123456732", types.DefaultUserServer)
	fa := &fakeClient{}
	m.add(acctA, fa)

	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: contact, Sender: contact},
			ID:            "m1",
			PushName:      "+62••••••32",
			Timestamp:     time.Unix(1000, 0),
		},
		Message: &waE2E.Message{Conversation: proto.String("hey")},
	}
	if err := m.handleEvent(ctx, acctA, fa, evt); err != nil {
		t.Fatal(err)
	}

	name, err := db.ChatName(acctA, contact.String())
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		t.Errorf("chats.name = %q, want empty string", name)
	}

	people, err := db.People(acctA)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range people {
		if p.JID == contact.String() && p.PushName != "" {
			t.Errorf("people.push_name = %q, want empty string", p.PushName)
		}
	}
}

func TestProfilePictureUnchangedAndMissing(t *testing.T) {
	m, _ := newTestManager(t)
	userA := types.NewJID("628111111111", types.DefaultUserServer)
	userB := types.NewJID("628222222222", types.DefaultUserServer)
	userC := types.NewJID("628333333333", types.DefaultUserServer)
	userD := types.NewJID("628444444444", types.DefaultUserServer)

	fa := &fakeClient{
		picInfo: map[types.JID]*types.ProfilePictureInfo{
			userA: {ID: "pic123", URL: "https://example.com/pic123.jpg"},
			userB: {ID: "pic456", URL: "https://example.com/pic456.jpg"},
		},
		picErr: map[types.JID]error{
			userC: whatsmeow.ErrProfilePictureNotSet,
			userD: whatsmeow.ErrProfilePictureUnauthorized,
		},
	}
	m.add(acctA, fa)

	// 1. Fetching known ID "pic123" for userA -> unchanged (ok false, nil error)
	pic, ok, err := m.ProfilePicture(ctx, acctA, userA.String(), "pic123", false)
	if err != nil {
		t.Fatalf("ProfilePicture userA known ID: %v", err)
	}
	if ok {
		t.Fatalf("ProfilePicture userA: expected ok=false (unchanged), got ok=true, pic=%+v", pic)
	}

	// 2. Fetching with empty known ID for userB -> changed (ok true, pic returned)
	pic, ok, err = m.ProfilePicture(ctx, acctA, userB.String(), "", true)
	if err != nil {
		t.Fatalf("ProfilePicture userB: %v", err)
	}
	if !ok || pic.ID != "pic456" || pic.URL != "https://example.com/pic456.jpg" {
		t.Fatalf("ProfilePicture userB: got ok=%v, pic=%+v; want ok=true, ID=pic456", ok, pic)
	}

	// 3. User with ErrProfilePictureNotSet -> ErrNoPicture
	_, _, err = m.ProfilePicture(ctx, acctA, userC.String(), "", false)
	if !errors.Is(err, ErrNoPicture) {
		t.Fatalf("ProfilePicture userC: got err=%v, want ErrNoPicture", err)
	}

	// 4. User with ErrProfilePictureUnauthorized -> ErrNoPicture
	_, _, err = m.ProfilePicture(ctx, acctA, userD.String(), "", false)
	if !errors.Is(err, ErrNoPicture) {
		t.Fatalf("ProfilePicture userD: got err=%v, want ErrNoPicture", err)
	}
}

func TestGroupDetailsUpsertsMembers(t *testing.T) {
	m, db := newTestManager(t)
	group := types.NewJID("12345-67890", types.GroupServer)
	member1 := types.NewJID("6281111111", types.DefaultUserServer)
	member2LID := types.NewJID("9988776655", types.HiddenUserServer)
	member2PN := types.NewJID("6282222222", types.DefaultUserServer)

	fa := &fakeClient{
		groups: map[types.JID]*types.GroupInfo{
			group: {
				JID:        group,
				GroupName:  types.GroupName{Name: "Book Club"},
				GroupTopic: types.GroupTopic{Topic: "Reading books"},
				Participants: []types.GroupParticipant{
					{
						JID:     member1,
						IsAdmin: true,
					},
					{
						JID:         member2LID,
						PhoneNumber: member2PN,
						IsAdmin:     false,
					},
				},
			},
		},
	}
	m.add(acctA, fa)

	details, err := m.GroupDetails(ctx, acctA, group.String())
	if err != nil {
		t.Fatalf("GroupDetails: %v", err)
	}
	if details.Subject != "Book Club" || details.Description != "Reading books" {
		t.Errorf("GroupDetails subject/desc = %q / %q; want %q / %q", details.Subject, details.Description, "Book Club", "Reading books")
	}
	if len(details.Members) != 2 {
		t.Fatalf("GroupDetails members len = %d, want 2", len(details.Members))
	}
	if details.Members[0].JID != member1.String() || !details.Members[0].Admin {
		t.Errorf("Member 0 = %+v, want JID=%s Admin=true", details.Members[0], member1.String())
	}
	if details.Members[1].JID != member2LID.String() || details.Members[1].Admin {
		t.Errorf("Member 1 = %+v, want JID=%s Admin=false", details.Members[1], member2LID.String())
	}

	people, err := db.People(acctA)
	if err != nil {
		t.Fatal(err)
	}
	peopleMap := make(map[string]store.Person)
	for _, p := range people {
		peopleMap[p.JID] = p
	}
	p1, ok1 := peopleMap[member1.String()]
	if !ok1 {
		t.Errorf("member1 not found in people table")
	} else if p1.PNJID != member1.String() {
		// p1 is phone JID
	}
	p2, ok2 := peopleMap[member2LID.String()]
	if !ok2 {
		t.Errorf("member2 not found in people table")
	} else if p2.PNJID != member2PN.String() {
		t.Errorf("member2 PNJID = %q, want %q", p2.PNJID, member2PN.String())
	}
}

func TestSendGIFCarriesPreview(t *testing.T) {
	m, _ := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	mp4 := filepath.Join(t.TempDir(), "test.mp4")
	if err := os.WriteFile(mp4, []byte("mp4 bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	preview := []byte("jpeg preview bytes")
	_, err := m.SendGIF(ctx, acctA, alice.String(), mp4, preview, 320, 240)
	if err != nil {
		t.Fatal(err)
	}
	if len(fa.sent) != 1 {
		t.Fatalf("sent messages count = %d, want 1", len(fa.sent))
	}
	v := fa.sent[0].GetVideoMessage()
	if v == nil {
		t.Fatal("sent message has no VideoMessage")
	}
	if !v.GetGifPlayback() {
		t.Errorf("GifPlayback = false, want true")
	}
	if string(v.GetJPEGThumbnail()) != "jpeg preview bytes" {
		t.Errorf("JPEGThumbnail = %q, want %q", string(v.GetJPEGThumbnail()), "jpeg preview bytes")
	}
	if v.GetWidth() != 320 || v.GetHeight() != 240 {
		t.Errorf("Width x Height = %d x %d, want 320 x 240", v.GetWidth(), v.GetHeight())
	}
}

func TestPictureEventResetsAvatarAndEmits(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)

	// Pre-seed an avatar
	if err := db.SetAvatar(acctA, alice.String(), "old_pic", 123456); err != nil {
		t.Fatal(err)
	}

	var avatarEvents []AvatarEvent
	m.AddListener(func(e any) {
		if ae, ok := e.(AvatarEvent); ok {
			avatarEvents = append(avatarEvents, ae)
		}
	})

	evt := &events.Picture{
		JID: alice,
	}
	if err := m.handleEvent(ctx, acctA, fa, evt); err != nil {
		t.Fatal(err)
	}

	people, err := db.People(acctA)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 1 || people[0].AvatarID != "" || people[0].AvatarTS != 0 {
		t.Errorf("expected avatar cleared; got %+v", people)
	}
	if len(avatarEvents) != 1 || avatarEvents[0] != (AvatarEvent{AccountID: acctA, JID: alice.String()}) {
		t.Errorf("expected AvatarEvent; got %+v", avatarEvents)
	}
}

func TestAboutReturnsStatus(t *testing.T) {
	m, _ := newTestManager(t)
	fa := &fakeClient{
		userInfo: map[types.JID]types.UserInfo{
			alice: {Status: "Busy coding"},
		},
	}
	m.add(acctA, fa)

	status, err := m.About(ctx, acctA, alice.String())
	if err != nil {
		t.Fatal(err)
	}
	if status != "Busy coding" {
		t.Errorf("About = %q, want %q", status, "Busy coding")
	}
}

func TestFetchPictureAndLimit(t *testing.T) {
	m, _ := newTestManager(t)

	// 1. Success fetch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("image-data"))
	}))
	defer srv.Close()

	data, err := m.FetchPicture(ctx, srv.URL)
	if err != nil {
		t.Fatalf("FetchPicture: %v", err)
	}
	if string(data) != "image-data" {
		t.Errorf("got %q, want %q", string(data), "image-data")
	}

	// 2. HTTP error
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer errSrv.Close()

	_, err = m.FetchPicture(ctx, errSrv.URL)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}

	// 3. Exceeds 2 MB limit
	bigSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, (2<<20)+10))
	}))
	defer bigSrv.Close()

	_, err = m.FetchPicture(ctx, bigSrv.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds 2 MB") {
		t.Fatalf("expected exceeds 2 MB error, got: %v", err)
	}
}

func TestContactsSyncOnConnect(t *testing.T) {
	m, db := newTestManager(t)
	bob := types.NewJID("6285555555", types.DefaultUserServer)
	masked := types.NewJID("6286666666", types.DefaultUserServer)

	fa := &fakeClient{
		contacts: map[types.JID]types.ContactInfo{
			alice:  {FullName: "Alice Wonderland"},
			bob:    {FullName: "Bob Builder"},
			masked: {FullName: "+62••••••66"},
		},
	}
	m.add(acctA, fa)

	var peopleEvents []PeopleEvent
	m.AddListener(func(e any) {
		if pe, ok := e.(PeopleEvent); ok {
			peopleEvents = append(peopleEvents, pe)
		}
	})

	if err := m.handleEvent(ctx, acctA, fa, &events.Connected{}); err != nil {
		t.Fatal(err)
	}

	people, err := db.People(acctA)
	if err != nil {
		t.Fatal(err)
	}
	peopleMap := make(map[string]store.Person)
	for _, p := range people {
		peopleMap[p.JID] = p
	}
	if p, ok := peopleMap[alice.String()]; !ok || p.FullName != "Alice Wonderland" {
		t.Errorf("alice not synced properly: %+v", p)
	}
	if p, ok := peopleMap[bob.String()]; !ok || p.FullName != "Bob Builder" {
		t.Errorf("bob not synced properly: %+v", p)
	}
	if _, ok := peopleMap[masked.String()]; ok {
		t.Errorf("masked contact should not be synced")
	}
	if len(peopleEvents) != 1 || peopleEvents[0].AccountID != acctA {
		t.Errorf("PeopleEvent count = %d, want 1", len(peopleEvents))
	}
}

func TestInboundQuotedMessageStored(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	group := types.NewJID("12345-67890", types.GroupServer)
	quote := func(participant *string) *waE2E.Message {
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("reply"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("xyz"), Participant: participant,
				QuotedMessage: &waE2E.Message{Conversation: proto.String("orig")}},
		}}
	}
	inGroup := incoming(group, "g1", "")
	inGroup.Message = quote(proto.String("6284444444444@s.whatsapp.net"))
	inDM := incoming(alice, "d1", "")
	inDM.Message = quote(nil) // a DM may leave the participant out
	for _, evt := range []*events.Message{inGroup, inDM} {
		if err := m.handleEvent(ctx, acctA, fa, evt); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := db.GetMessage(acctA, group.String(), "g1")
	if err != nil || !ok {
		t.Fatalf("GetMessage: ok=%v err=%v", ok, err)
	}
	if got.QuotedID != "xyz" || got.QuotedSender != "6284444444444@s.whatsapp.net" || got.QuotedBody != "orig" || got.QuotedType != TypeText {
		t.Fatalf("group quote = %+v", got)
	}
	got, _, _ = db.GetMessage(acctA, alice.String(), "d1")
	if got.QuotedID != "xyz" || got.QuotedSender != alice.String() {
		t.Fatalf("DM quote = %+v, want sender %s", got, alice)
	}
}

func TestOutboundReplyContextInfo(t *testing.T) {
	m, _ := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	// The chat has disappearing messages on: the timer and the quote must share one context info.
	in := incoming(alice, "in1", "")
	in.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("hi"),
		ContextInfo: &waE2E.ContextInfo{Expiration: proto.Uint32(3600)}}}
	if err := m.handleEvent(ctx, acctA, fa, in); err != nil {
		t.Fatal(err)
	}
	reply := &store.Message{ID: "in1", SenderJID: alice.String(), Type: TypeText, Body: "hi"}
	sent, err := m.SendText(ctx, acctA, alice.String(), "hello back", reply, nil)
	if err != nil {
		t.Fatal(err)
	}
	ci := fa.sent[0].GetExtendedTextMessage().GetContextInfo()
	if ci.GetStanzaID() != "in1" || ci.GetParticipant() != alice.String() || ci.GetQuotedMessage().GetConversation() != "hi" {
		t.Fatalf("context info = %v", ci)
	}
	if ci.GetExpiration() != 3600 {
		t.Fatalf("expiration = %d, want 3600", ci.GetExpiration())
	}
	if sent.QuotedID != "in1" || sent.QuotedBody != "hi" || sent.QuotedSender != alice.String() {
		t.Fatalf("stored reply = %+v", sent)
	}

	// A quoted image travels as an image, from its stored media sub-message.
	meta, _ := proto.Marshal(&waE2E.ImageMessage{Caption: proto.String("cat")})
	if _, err := m.SendText(ctx, acctA, alice.String(), "nice", &store.Message{ID: "in2", SenderJID: alice.String(), Type: TypeImage, MediaMeta: meta}, nil); err != nil {
		t.Fatal(err)
	}
	if got := fa.sent[1].GetExtendedTextMessage().GetContextInfo().GetQuotedMessage().GetImageMessage().GetCaption(); got != "cat" {
		t.Fatalf("quoted image caption = %q", got)
	}
}

func TestOutboundMentionAllExpansion(t *testing.T) {
	m, _ := newTestManager(t)
	group := types.NewJID("12345-67890", types.GroupServer)
	m1 := types.NewJID("6281111111", types.DefaultUserServer)
	m2 := types.NewJID("9988776655", types.HiddenUserServer)
	fa := &fakeClient{groups: map[types.JID]*types.GroupInfo{group: {JID: group,
		Participants: []types.GroupParticipant{{JID: m1}, {JID: m2}}}}}
	m.add(acctA, fa)

	mentioned := func(i int) []string { return fa.sent[i].GetExtendedTextMessage().GetContextInfo().GetMentionedJID() }
	if _, err := m.SendText(ctx, acctA, group.String(), "hi @ALL and @6281111111", nil, []string{m1.String()}); err != nil {
		t.Fatal(err)
	}
	if got := mentioned(0); !slices.Equal(got, []string{m1.String(), m2.String()}) {
		t.Fatalf("mentioned = %v, want both members once", got)
	}
	if _, err := m.SendText(ctx, acctA, group.String(), "mail me@all.example or call@everyone", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := mentioned(1); len(got) != 0 {
		t.Fatalf("mentioned = %v for text without an @all word", got)
	}
	if _, err := m.SendText(ctx, acctA, group.String(), "@everyone", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := mentioned(2); len(got) != 2 || fa.groupCalls() != 1 {
		t.Fatalf("mentioned = %v after %d group lookups, want 2 after 1 (cached)", got, fa.groupCalls())
	}
	// A membership change drops the cached participants.
	if err := m.handleEvent(ctx, acctA, fa, &events.GroupInfo{JID: group, Join: []types.JID{alice}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GroupParticipants(ctx, acctA, group.String()); err != nil || fa.groupCalls() != 2 {
		t.Fatalf("group lookups = %d (%v), want 2 after a join", fa.groupCalls(), err)
	}
	// @all in a direct chat is just text.
	if _, err := m.SendText(ctx, acctA, alice.String(), "@all", nil, nil); err != nil {
		t.Fatal(err)
	}
	if fa.sent[3].GetConversation() != "@all" {
		t.Fatalf("DM @all = %v", fa.sent[3])
	}
}

func TestQuotedSenderIsStoredAsThePhoneNumber(t *testing.T) {
	m, db := newTestManager(t)
	group := types.NewJID("12345-67890", types.GroupServer)
	lid := types.NewJID("123456789012345", types.HiddenUserServer)
	fa := &fakeClient{pns: map[types.JID]types.JID{lid: alice}}
	m.add(acctA, fa)
	reply := func(id, quoted, participant string) *events.Message {
		e := incoming(group, id, "")
		e.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("re"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String(quoted), Participant: proto.String(participant),
				QuotedMessage: &waE2E.Message{Conversation: proto.String("orig")}}}}
		return e
	}
	mine, err := m.SendText(ctx, acctA, group.String(), "mine", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// someone quotes our message by an LID nobody can map: the stored original names its sender
	for _, e := range []*events.Message{reply("r1", mine.ID, "999999999999999@lid"), reply("r2", "not-stored", lid.String())} {
		if err := m.handleEvent(ctx, acctA, fa, e); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _ := db.GetMessage(acctA, group.String(), "r1"); got.QuotedSender != acctA+"@s.whatsapp.net" {
		t.Fatalf("quote of our message credited to %q", got.QuotedSender)
	}
	if got, _, _ := db.GetMessage(acctA, group.String(), "r2"); got.QuotedSender != alice.String() {
		t.Fatalf("quote by LID credited to %q, want %s", got.QuotedSender, alice)
	}

	// replying to a message that arrived by LID addresses its sender the way it arrived
	in := store.Message{ID: "g1", SenderJID: alice.String(), WireSenderJID: lid.String(), Type: TypeText, Body: "hi"}
	sent, err := m.SendText(ctx, acctA, group.String(), "ok", &in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := fa.sent[len(fa.sent)-1].GetExtendedTextMessage().GetContextInfo().GetParticipant(); p != lid.String() {
		t.Fatalf("participant = %q, want the LID %s", p, lid)
	}
	if sent.QuotedSender != alice.String() {
		t.Fatalf("own reply stored with quoted sender %q", sent.QuotedSender)
	}
}

func TestSendTextWithPreview(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)

	thumb := []byte{0xff, 0xd8, 0xff, 0xe0}
	preview := &LinkPreview{
		MatchedText:  "https://example.com",
		CanonicalURL: "https://example.com/canonical",
		Title:        "Example Title",
		Description:  "Example Desc",
		Thumb:        thumb,
	}
	reply := &store.Message{ID: "in1", SenderJID: alice.String(), Type: TypeText, Body: "hi"}
	sent, err := m.SendTextWithPreview(ctx, acctA, alice.String(), "visit https://example.com", reply, []string{alice.String()}, preview)
	if err != nil {
		t.Fatal(err)
	}
	if len(fa.sent) != 1 {
		t.Fatalf("sent count = %d, want 1", len(fa.sent))
	}
	etm := fa.sent[0].GetExtendedTextMessage()
	if etm == nil {
		t.Fatalf("expected ExtendedTextMessage, got %v", fa.sent[0])
	}
	if etm.GetText() != "visit https://example.com" ||
		etm.GetMatchedText() != "https://example.com" ||
		etm.GetTitle() != "Example Title" ||
		etm.GetDescription() != "Example Desc" ||
		!slices.Equal(etm.GetJPEGThumbnail(), thumb) ||
		etm.GetPreviewType() != waE2E.ExtendedTextMessage_NONE {
		t.Fatalf("unexpected ExtendedTextMessage fields: %+v", etm)
	}
	if ci := etm.GetContextInfo(); ci.GetStanzaID() != "in1" || !slices.Equal(ci.GetMentionedJID(), []string{alice.String()}) {
		t.Fatalf("unexpected ContextInfo: %+v", ci)
	}
	if sent.LinkURL != "https://example.com" ||
		sent.LinkCanonical != "https://example.com/canonical" ||
		sent.LinkTitle != "Example Title" ||
		sent.LinkDesc != "Example Desc" ||
		!slices.Equal(sent.LinkThumb, thumb) {
		t.Fatalf("stored sent message lost link preview: %+v", sent)
	}
	stored, ok, err := db.GetMessage(acctA, alice.String(), sent.ID)
	if err != nil || !ok {
		t.Fatalf("GetMessage: ok=%v err=%v", ok, err)
	}
	if stored.LinkURL != "https://example.com" ||
		stored.LinkCanonical != "https://example.com/canonical" ||
		stored.LinkTitle != "Example Title" ||
		stored.LinkDesc != "Example Desc" ||
		!slices.Equal(stored.LinkThumb, thumb) {
		t.Fatalf("db message lost link preview: %+v", stored)
	}

	// nil preview keeps plain Conversation when no context info is needed.
	if _, err := m.SendTextWithPreview(ctx, acctA, alice.String(), "plain", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if fa.sent[1].GetConversation() != "plain" || fa.sent[1].GetExtendedTextMessage() != nil {
		t.Fatalf("nil preview should send plain Conversation, got %v", fa.sent[1])
	}
}

func TestCheckPhonesCachesSessionResults(t *testing.T) {
	m, _ := newTestManager(t)
	lid := types.NewJID("1234567890", types.HiddenUserServer)
	bob := types.NewJID("6284444444444", types.DefaultUserServer)
	fa := &fakeClient{
		isOnWAResp: map[string]types.IsOnWhatsAppResponse{
			"+6283333333333": {Query: "+6283333333333", IsIn: true, JID: alice},
			"+6289999999999": {Query: "+6289999999999", IsIn: false},
			"+6284444444444": {Query: "+6284444444444", IsIn: true, JID: lid, PhoneNumber: bob},
			"+6286666666666": {Query: "+6286666666666", IsIn: true, JID: types.NewJID("777", types.HiddenUserServer)},
		},
	}
	fb := &fakeClient{
		isOnWAResp: map[string]types.IsOnWhatsAppResponse{
			"+6283333333333": {Query: "+6283333333333", IsIn: false},
		},
	}
	m.add(acctA, fa)
	m.add(acctB, fb)

	got, err := m.CheckPhones(ctx, acctA, []string{"+6283333333333", "+6289999999999", "+6284444444444", "+6286666666666"})
	if err != nil {
		t.Fatal(err)
	}
	if got["+6286666666666"] != (PhoneStatus{Registered: true, JID: "6286666666666@s.whatsapp.net"}) {
		t.Fatalf("+6286666666666 (LID only) = %+v, want the number's own JID", got["+6286666666666"])
	}
	if got["+6283333333333"] != (PhoneStatus{Registered: true, JID: alice.String()}) {
		t.Fatalf("+6283333333333 = %+v", got["+6283333333333"])
	}
	if got["+6289999999999"] != (PhoneStatus{Registered: false}) {
		t.Fatalf("+6289999999999 = %+v", got["+6289999999999"])
	}
	if got["+6284444444444"] != (PhoneStatus{Registered: true, JID: bob.String()}) {
		t.Fatalf("+6284444444444 (LID+PN) = %+v, want %s", got["+6284444444444"], bob)
	}
	if len(fa.isOnWACalls) != 1 {
		t.Fatalf("IsOnWhatsApp calls = %d, want 1", len(fa.isOnWACalls))
	}

	// Second lookup for cached numbers must not call IsOnWhatsApp again.
	got2, err := m.CheckPhones(ctx, acctA, []string{"+6283333333333", "+6289999999999"})
	if err != nil {
		t.Fatal(err)
	}
	if got2["+6283333333333"] != (PhoneStatus{Registered: true, JID: alice.String()}) ||
		got2["+6289999999999"] != (PhoneStatus{Registered: false}) {
		t.Fatalf("cached lookup = %+v", got2)
	}
	if len(fa.isOnWACalls) != 1 {
		t.Fatalf("IsOnWhatsApp calls after cache hit = %d, want 1", len(fa.isOnWACalls))
	}

	// Cache is scoped per account.
	gotB, err := m.CheckPhones(ctx, acctB, []string{"+6283333333333"})
	if err != nil {
		t.Fatal(err)
	}
	if gotB["+6283333333333"] != (PhoneStatus{Registered: false}) || len(fb.isOnWACalls) != 1 {
		t.Fatalf("acctB lookup = %+v (calls=%d)", gotB, len(fb.isOnWACalls))
	}

	// Transient errors are not cached.
	fa.isOnWAErr = errors.New("network down")
	if _, err := m.CheckPhones(ctx, acctA, []string{"+6285555555555"}); err == nil {
		t.Fatal("expected error when IsOnWhatsApp fails")
	}
	fa.isOnWAErr = nil
	if got3, err := m.CheckPhones(ctx, acctA, []string{"+6285555555555"}); err != nil || got3["+6285555555555"].Registered {
		t.Fatalf("retry after error = %+v, err=%v", got3, err)
	}
}
