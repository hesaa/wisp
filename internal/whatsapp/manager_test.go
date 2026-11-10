package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	download    []byte
	receipts    []markRead              // one per MarkRead call
	pns         map[types.JID]types.JID // LID -> phone number
	names       map[types.JID]string    // address-book names
	contacts    map[types.JID]types.ContactInfo
}

func (f *fakeClient) AllContacts(context.Context) (map[types.JID]types.ContactInfo, error) {
	return f.contacts, nil
}

type markRead struct {
	chat, sender types.JID
	ids          []string
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

func (f *fakeClient) MarkRead(_ context.Context, ids []types.MessageID, _ time.Time, chat, sender types.JID, _ ...types.ReceiptType) error {
	f.receipts = append(f.receipts, markRead{chat, sender, ids})
	return nil
}

func (f *fakeClient) UploadReader(_ context.Context, r io.Reader, tmp io.ReadWriteSeeker, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
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
	var got []any
	m.AddListener(func(e any) { got = append(got, e) })

	for range 2 { // the second is a redelivery
		if err := m.handleEvent(ctx, acctA, fa, incoming(alice, "m1", "hi")); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 1 {
		t.Fatalf("listener got %d events, want 1 (a redelivery must not re-emit)", len(got))
	}
	ev, ok := got[0].(MessageEvent)
	if !ok || ev.AccountID != acctA || ev.Message.AccountID != acctA || ev.Message.Body != "hi" ||
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
	sent, err := m.SendText(ctx, acctA, alice.String(), "hello")
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
		"SendText": func() error { _, err := m.SendText(ctx, "nope", alice.String(), "x"); return err },
		"Logout":   func() error { return m.Logout(ctx, "nope") },
		"DownloadMedia": func() error {
			_, err := m.DownloadMedia(ctx, store.Message{AccountID: "nope", Type: TypeImage})
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
	if err := errors.Join(os.WriteFile(png, []byte("png bytes"), 0o600), os.WriteFile(txt, []byte("some notes"), 0o600)); err != nil {
		t.Fatal(err)
	}

	img, err := m.SendMedia(ctx, acctA, alice.String(), png, "a photo")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := m.SendMedia(ctx, acctA, alice.String(), txt, "")
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
	if page, _ := db.GetChatMessages(acctA, alice.String(), 0); len(page) != 2 {
		t.Fatalf("stored %d outgoing", len(page))
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
	if err := m.MarkRead(ctx, msgs); err != nil {
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
	if err := m.MarkRead(ctx, page); err != nil {
		t.Fatal(err)
	}
	if r := fa.receipts; len(r) != 1 || r[0].chat != lid || r[0].sender.String() != lid.String() || statusOf(t, db, acctA, "l1") != store.StatusRead {
		t.Fatalf("read receipt %+v (want it addressed to the LID it came from)", r)
	}
	// a receipt addressed by LID alone is resolved through the account's LID map
	fa.pns = map[types.JID]types.JID{lid: alice}
	sent, err := m.SendText(ctx, acctA, alice.String(), "reply")
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
	own, err := m.SendText(ctx, acctA, group.String(), "mine")
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
	poll.Message = &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("lunch?")}}
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
