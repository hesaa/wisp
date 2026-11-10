package whatsapp

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/store"
)

func unreadOf(t *testing.T, db *spyStore, acct string) int {
	t.Helper()
	chats, err := db.ListChats(acct)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range chats {
		n += c.Unread
	}
	return n
}

func TestReadElsewhereLowersOnlyThatAccount(t *testing.T) {
	m, db := newTestManager(t)
	fa, fb := &fakeClient{}, &fakeClient{}
	m.add(acctA, fa)
	m.add(acctB, fb)
	for i, id := range []string{"in1", "in2", "in3"} {
		for acct, cli := range map[string]*fakeClient{acctA: fa, acctB: fb} {
			evt := incoming(alice, id, "yo")
			evt.Info.Timestamp = time.Unix(int64(1000+i), 0)
			if err := m.handleEvent(ctx, acct, cli, evt); err != nil {
				t.Fatal(err)
			}
		}
	}
	var got []any
	m.AddListener(func(e any) { got = append(got, e) })

	// read on the phone with read receipts off: a read-self receipt from the user's own device
	self := &events.Receipt{MessageSource: types.MessageSource{Chat: alice, Sender: types.NewJID(acctA, types.DefaultUserServer), IsFromMe: true},
		MessageIDs: []string{"in1"}, Type: types.ReceiptTypeReadSelf}
	if err := m.handleEvent(ctx, acctA, fa, self); err != nil {
		t.Fatal(err)
	}
	if ev, ok := got[0].(ReceiptEvent); !ok || !ev.FromMe || ev.AccountID != acctA || ev.Status != store.StatusRead {
		t.Fatalf("read-self event %#v", got[0])
	}
	if a, b := unreadOf(t, db, acctA), unreadOf(t, db, acctB); a != 2 || b != 3 {
		t.Fatalf("after read-self: A unread %d (want 2), B unread %d (want 3)", a, b)
	}

	// the chat marked read on another device up to in2 (app state): in3 stays unread
	mark := &events.MarkChatAsRead{JID: alice, Timestamp: time.Unix(5000, 0), Action: &waSyncAction.MarkChatAsReadAction{
		Read: proto.Bool(true), MessageRange: &waSyncAction.SyncActionMessageRange{LastMessageTimestamp: proto.Int64(1001)}}}
	if err := m.handleEvent(ctx, acctA, fa, mark); err != nil {
		t.Fatal(err)
	}
	if ev, ok := got[len(got)-1].(ChatReadEvent); !ok || ev.AccountID != acctA || ev.ChatJID != alice.String() {
		t.Fatalf("mark-read event %#v", got[len(got)-1])
	}
	if a, b := unreadOf(t, db, acctA), unreadOf(t, db, acctB); a != 1 || b != 3 {
		t.Fatalf("after MarkChatAsRead: A unread %d (want 1), B unread %d (want 3)", a, b)
	}

	// marked unread elsewhere: nothing changes here
	n := len(got)
	unmark := &events.MarkChatAsRead{JID: alice, Action: &waSyncAction.MarkChatAsReadAction{Read: proto.Bool(false)}}
	if err := m.handleEvent(ctx, acctA, fa, unmark); err != nil || len(got) != n || unreadOf(t, db, acctA) != 1 {
		t.Fatalf("mark-unread changed something: err %v events %d", err, len(got)-n)
	}

	// no range: everything up to when it was marked
	mark.Action.MessageRange = nil
	if err := m.handleEvent(ctx, acctA, fa, mark); err != nil || unreadOf(t, db, acctA) != 0 || unreadOf(t, db, acctB) != 3 {
		t.Fatalf("mark-read without range: err %v A %d B %d", err, unreadOf(t, db, acctA), unreadOf(t, db, acctB))
	}

	// a contact's read receipt on the user's own message is not "read elsewhere"
	if err := m.handleEvent(ctx, acctA, fa, &events.Receipt{MessageSource: types.MessageSource{Chat: alice, Sender: alice},
		MessageIDs: []string{"in3"}, Type: types.ReceiptTypeRead}); err != nil {
		t.Fatal(err)
	}
	if ev := got[len(got)-1].(ReceiptEvent); ev.FromMe {
		t.Fatalf("contact's receipt marked as from me: %#v", ev)
	}
}

func TestGroupNamesFromGroupEvents(t *testing.T) {
	m, db := newTestManager(t)
	fa, fb := &fakeClient{}, &fakeClient{}
	m.add(acctA, fa)
	m.add(acctB, fb)
	var got []any
	m.AddListener(func(e any) { got = append(got, e) })
	group := types.NewJID("120363000000000001", types.GroupServer)

	joined := &events.JoinedGroup{GroupInfo: types.GroupInfo{JID: group, GroupName: types.GroupName{Name: "Team"}}}
	if err := m.handleEvent(ctx, acctA, fa, joined); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.ChatName(acctA, group.String()); n != "Team" {
		t.Fatalf("joined group named %q", n)
	}
	if n, _ := db.ChatName(acctB, group.String()); n != "" {
		t.Fatalf("B's chat named %q", n)
	}
	renamed := &events.GroupInfo{JID: group, Name: &types.GroupName{Name: "Team 2"}}
	if err := m.handleEvent(ctx, acctA, fa, renamed); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.ChatName(acctA, group.String()); n != "Team 2" {
		t.Fatalf("renamed group named %q", n)
	}
	other := &events.GroupInfo{JID: group, Topic: &types.GroupTopic{Topic: "x"}} // no name change
	if err := m.handleEvent(ctx, acctA, fa, other); err != nil {
		t.Fatal(err)
	}
	want := []any{ChatNamedEvent{AccountID: acctA, ChatJID: group.String(), Name: "Team"},
		ChatNamedEvent{AccountID: acctA, ChatJID: group.String(), Name: "Team 2"}}
	if !slices.Equal(got, want) {
		t.Fatalf("events %#v", got)
	}
}

func TestUnnamedGroupNamedOnceFromWhatsApp(t *testing.T) {
	m, db := newTestManager(t)
	group := types.NewJID("120363000000000002", types.GroupServer)
	fa := &fakeClient{groupNames: map[types.JID]string{group: "Family"}}
	m.add(acctA, fa)
	named := make(chan ChatNamedEvent, 4)
	m.AddListener(func(e any) {
		if ev, ok := e.(ChatNamedEvent); ok {
			named <- ev
		}
	})
	for _, id := range []string{"g1", "g2"} {
		if err := m.handleEvent(ctx, acctA, fa, incoming(group, id, "hi")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case ev := <-named:
		if ev != (ChatNamedEvent{AccountID: acctA, ChatJID: group.String(), Name: "Family"}) {
			t.Fatalf("event %#v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the group was never named")
	}
	if n, _ := db.ChatName(acctA, group.String()); n != "Family" {
		t.Fatalf("group named %q", n)
	}
	if err := m.handleEvent(ctx, acctA, fa, incoming(group, "g3", "again")); err != nil {
		t.Fatal(err)
	}
	m.groupFetches.Wait()
	if calls := fa.groupCalls(); calls != 1 {
		t.Fatalf("GetGroupInfo called %d times, want once", calls)
	}
}

func TestPushNameSettingAsksForPresence(t *testing.T) {
	m, _ := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	var got []any
	m.AddListener(func(e any) { got = append(got, e) })
	if err := m.handleEvent(ctx, acctA, fa, &events.PushNameSetting{Action: &waSyncAction.PushNameSetting{Name: proto.String("Me")}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []any{PushNameEvent{AccountID: acctA}}) {
		t.Fatalf("events %#v", got)
	}
}

func TestDisappearingMessagesCarryTheirExpiry(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	bob := types.NewJID("6284444444444", types.DefaultUserServer)
	evt := incoming(alice, "e1", "")
	evt.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("poof"),
		ContextInfo: &waE2E.ContextInfo{Expiration: proto.Uint32(86400)}}}
	evt.IsEphemeral = true
	if err := m.handleEvent(ctx, acctA, fa, evt); err != nil {
		t.Fatal(err)
	}
	page, _ := db.GetChatMessages(acctA, alice.String(), 0)
	if len(page) != 1 || page[0].ExpiresAt != 1000+86400 {
		t.Fatalf("stored %+v", page)
	}

	// a reply in that chat disappears too, for everyone
	sent, err := m.SendText(ctx, acctA, alice.String(), "ok")
	if err != nil {
		t.Fatal(err)
	}
	ext := fa.sent[0].GetExtendedTextMessage()
	if ext.GetText() != "ok" || ext.GetContextInfo().GetExpiration() != 86400 || sent.ExpiresAt != 2000+86400 || sent.Body != "ok" {
		t.Fatalf("sent %v stored %+v", fa.sent[0], sent)
	}
	// a chat without a timer gets a plain message
	plain, err := m.SendText(ctx, acctA, bob.String(), "hi")
	if err != nil || fa.sent[1].GetConversation() != "hi" || plain.ExpiresAt != 0 {
		t.Fatalf("plain send %v stored %+v err %v", fa.sent[1], plain, err)
	}
}

func TestStartListsAccountsBeforeConnecting(t *testing.T) {
	m, _ := newTestManager(t)
	dev := m.container.NewDevice()
	dev.ID = &types.JID{User: acctA, Device: 7, Server: types.DefaultUserServer}
	sig, key := make([]byte, 64), make([]byte, 32) // a stored session needs these; nothing reads them here
	dev.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{0}, AccountSignature: sig, AccountSignatureKey: key, DeviceSignature: sig}
	if err := m.container.PutDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	m.connect = func(*whatsmeow.Client) error {
		defer wg.Done()
		<-release // a slow handshake
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- m.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start waited for the connection")
	}
	if got := m.Accounts(); !slices.Equal(got, []string{acctA}) {
		t.Fatalf("accounts %v", got)
	}
	close(release)
	wg.Wait()
}

func TestDownloadedMediaCarriesTheMarkOfTheWeb(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{download: []byte("%PDF")}
	m.add(acctA, fa)
	evt := incoming(alice, "doc1", "")
	evt.Message = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("invoice.pdf"),
		Mimetype: proto.String("application/pdf"), DirectPath: proto.String("/x"), MediaKey: []byte("k")}}
	if err := m.handleEvent(ctx, acctA, fa, evt); err != nil {
		t.Fatal(err)
	}
	page, _ := db.GetChatMessages(acctA, alice.String(), 0)
	path, err := m.DownloadMedia(ctx, page[0])
	if err != nil {
		t.Fatal(err)
	}
	if zone, err := os.ReadFile(path + ":Zone.Identifier"); err != nil || string(zone) != "[ZoneTransfer]\r\nZoneId=3\r\n" {
		t.Fatalf("no Mark of the Web on %s: %q %v", path, zone, err)
	}
}

func TestMarkReadSelfOnly(t *testing.T) {
	m, db := newTestManager(t)
	fa := &fakeClient{}
	m.add(acctA, fa)
	msg := store.Message{ID: "a1", ChatJID: alice.String(), AccountID: acctA, SenderJID: alice.String(), Timestamp: 1, Status: store.StatusReceived}
	if err := db.InsertMessagesBatch([]store.Message{msg}); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkRead(ctx, []store.Message{msg}, true); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkRead(ctx, []store.Message{msg}, false); err != nil {
		t.Fatal(err)
	}
	if len(fa.receipts) != 2 || fa.receipts[0].typ != types.ReceiptTypeReadSelf || fa.receipts[1].typ != types.ReceiptTypeRead {
		t.Fatalf("receipts %+v", fa.receipts)
	}
}
