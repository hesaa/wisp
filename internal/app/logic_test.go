package app

import (
	"testing"

	"go.mau.fi/whatsmeow/types"

	"wa-go/internal/store"
)

const (
	acctA = "6281234567890"
	acctB = "6289876543210"
	alice = "6281111111111@s.whatsapp.net"
	bob   = "6282222222222@s.whatsapp.net"
)

func incoming(acct, chat string) store.Message {
	return store.Message{ID: "m1", AccountID: acct, ChatJID: chat, SenderJID: chat, Type: "text", Status: store.StatusReceived}
}

func TestRouteMessage(t *testing.T) {
	open := chatRef{acctA, alice}
	mine := incoming(acctA, alice)
	mine.FromMe, mine.Status = true, store.StatusSent
	read := incoming(acctA, bob)
	read.Status = store.StatusRead // an unsupported message, stored as read
	for _, c := range []struct {
		name       string
		m          store.Message
		foreground bool
		notifyOn   bool
		want       msgRoute
	}{
		{"open chat, window in front: shown and read at once", incoming(acctA, alice), true, true,
			msgRoute{sidebar: true, upsert: true, markRead: true}},
		{"open chat, window behind: shown, unread, toast", incoming(acctA, alice), false, true,
			msgRoute{sidebar: true, upsert: true, unread: true, notify: true}},
		{"other chat of the active account", incoming(acctA, bob), true, true,
			msgRoute{sidebar: true, unread: true, notify: true}},
		{"toasts off", incoming(acctA, bob), true, false,
			msgRoute{sidebar: true, unread: true}},
		{"sent by me", mine, false, true,
			msgRoute{sidebar: true, upsert: true}},
		{"already read", read, false, true,
			msgRoute{sidebar: true, notify: true}},
		{"same chat JID on another account: never the open chat", incoming(acctB, alice), true, true,
			msgRoute{unread: true, notify: true}},
		{"story of the active account", incoming(acctA, "status@broadcast"), false, true,
			msgRoute{stories: true}},
		{"story of another account", incoming(acctB, "status@broadcast"), false, true,
			msgRoute{}},
	} {
		if got := routeMessage(c.m, acctA, open, c.foreground, c.notifyOn); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestSettingGates(t *testing.T) {
	for v, want := range map[string]bool{"all": true, "contacts": true, "": true, "none": false} {
		if receiptsOn(v) != want {
			t.Errorf("receiptsOn(%q) = %v", v, !want)
		}
	}
	for v, want := range map[string]bool{"false": true, "": true, "true": false} {
		if typingOn(v) != want {
			t.Errorf("typingOn(ghost %q) = %v", v, !want)
		}
	}
}

func TestUnreadIncoming(t *testing.T) {
	mine := incoming(acctA, alice)
	mine.FromMe = true
	read := incoming(acctA, alice)
	read.Status = store.StatusRead
	got := unreadIncoming([]store.Message{mine, incoming(acctA, alice), read})
	if len(got) != 1 || got[0].FromMe || got[0].Status != store.StatusReceived {
		t.Fatalf("unreadIncoming = %+v", got)
	}
}

func TestPresenceDecisions(t *testing.T) {
	type step struct {
		visible, appearOffline, force bool
		want                          types.Presence
		send                          bool
	}
	for name, steps := range map[string][]step{
		"startup online, activations, hide to tray, restore": {
			{true, false, false, types.PresenceAvailable, true},
			{true, false, false, types.PresenceAvailable, false},  // another activation: nothing new
			{false, false, true, types.PresenceUnavailable, true}, // closed to the tray: at once
			{true, false, false, types.PresenceAvailable, true},   // restored
			{true, false, true, types.PresenceAvailable, true},    // reconnected: said again
		},
		"appear offline": {
			{true, true, false, types.PresenceUnavailable, true}, // startup
			{true, true, false, types.PresenceUnavailable, false},
			{false, true, true, types.PresenceUnavailable, true},
			{true, true, false, types.PresenceUnavailable, false}, // restored: still offline, already said
			{true, false, true, types.PresenceAvailable, true},    // setting turned off
		},
	} {
		var p presenceState
		for i, s := range steps {
			got, send := p.next(s.visible, s.appearOffline, s.force)
			if got != s.want || send != s.send {
				t.Errorf("%s, step %d: got %q send %v, want %q send %v", name, i, got, send, s.want, s.send)
			}
		}
	}
}

func TestGIFKeyResolution(t *testing.T) {
	if got := gifKey("user", "built-in"); got != "user" {
		t.Errorf("a key in Settings must win, got %q", got)
	}
	if got := gifKey("", "built-in"); got != "built-in" {
		t.Errorf("no key in Settings must use the built-in one, got %q", got)
	}
	if got := gifKey("", ""); got != "" {
		t.Errorf("no key at all, got %q", got)
	}
}

func TestTotalUnreadCountsListedAccountsOnly(t *testing.T) {
	unread := map[string]int{acctA: 3, acctB: 4, "6280000000000": 9} // the last was logged out
	if got := totalUnread(unread, []string{acctA, acctB}); got != 7 {
		t.Fatalf("totalUnread = %d, want 7", got)
	}
}

func TestInflightDedupesPerAccountChatAndMessage(t *testing.T) {
	f := inflight{}
	k := mediaKey{acctA, alice, "m1"}
	if !f.start(k) || f.start(k) {
		t.Fatal("a second request for a running download was not refused")
	}
	// the same message ID in another account (a group both numbers are in) or chat is its own download
	if !f.start(mediaKey{acctB, alice, "m1"}) || !f.start(mediaKey{acctA, "status@broadcast", "m1"}) {
		t.Fatal("downloads of other accounts or chats were refused")
	}
	f.done(k)
	if !f.start(k) {
		t.Fatal("a finished download cannot be requested again")
	}
}

func TestAccountSwitchClearsTheChatPane(t *testing.T) {
	v := view{active: acctA, unread: map[string]int{acctA: 2, acctB: 5}}
	v.openChat(chatRef{acctA, alice})
	v.page, v.loaded, v.pending = 3, true, []store.Message{incoming(acctA, alice)}
	v.openNext = chatRef{acctB, bob} // a toast for B was clicked
	v.switchAccount(acctB)
	if v.active != acctB || v.open != (chatRef{}) || v.page != 0 || v.loaded || v.pending != nil {
		t.Fatalf("after the switch: %+v", v)
	}
	if v.unread[acctA] != 2 || v.unread[acctB] != 5 {
		t.Fatalf("unread totals changed: %v", v.unread)
	}
	if got, ok := v.takeOpenNext(acctA); ok {
		t.Fatalf("a chat queued for B opened in A: %v", got)
	}
	if got, ok := v.takeOpenNext(acctB); !ok || got != bob {
		t.Fatalf("takeOpenNext(B) = %q, %v", got, ok)
	}
	if _, ok := v.takeOpenNext(acctB); ok {
		t.Fatal("the queued chat opened twice")
	}
}

func TestOpeningAChatStartsFresh(t *testing.T) {
	var v view
	v.openChat(chatRef{acctA, alice})
	v.page, v.loaded, v.pending = 2, true, []store.Message{incoming(acctA, alice)}
	v.openChat(chatRef{acctA, bob})
	if v.open != (chatRef{acctA, bob}) || v.page != 0 || v.loaded || v.pending != nil {
		t.Fatalf("after opening another chat: %+v", v)
	}
}
