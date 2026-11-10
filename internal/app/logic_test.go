package app

import (
	"testing"

	"go.mau.fi/whatsmeow/types"

	"wa-go/internal/store"
	"wa-go/internal/ui"
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

func TestStaleChatListNeverUndoesNewerChanges(t *testing.T) {
	v := view{active: acctA, unread: map[string]int{}}
	side := ui.NewSidebarModel()
	side.SetChats(acctA, []store.Chat{{JID: alice, Unread: 2}, {JID: bob, Unread: 1}})
	// arrive mirrors loadChats' answer: the list and the account total are taken only when fresh
	arrive := func(seq int, chats []store.Chat) (applied, again bool) {
		applied, again = v.reloadArrived(acctA, seq)
		if applied {
			side.SetChats(acctA, chats)
			v.unread[acctA] = side.Unread()
		}
		return applied, again
	}
	unreadOf := func(jid string) int {
		for _, c := range side.Chats() {
			if c.JID == jid {
				return c.Unread
			}
		}
		return -1
	}
	check := func(step string, aliceWant, bobWant, total int) {
		t.Helper()
		if unreadOf(alice) != aliceWant || unreadOf(bob) != bobWant || totalUnread(v.unread, []string{acctA}) != total {
			t.Fatalf("%s: alice %d, bob %d, total %d; want %d, %d, %d", step, unreadOf(alice), unreadOf(bob),
				totalUnread(v.unread, []string{acctA}), aliceWant, bobWant, total)
		}
	}

	// a reload starts; the user opens alice: read on screen at once, the store write under way
	seq := v.reloadStarted(acctA)
	side.MarkRead(alice)
	v.writeStarted(acctA)
	v.unread[acctA] = side.Unread()
	// the reload read the store before the write: dropped, asked again only once the write is done
	if applied, again := arrive(seq, []store.Chat{{JID: alice, Unread: 2}, {JID: bob, Unread: 1}}); applied || again {
		t.Fatalf("stale list during the write: applied %v, asked again at once %v", applied, again)
	}
	check("stale list during the write", 0, 1, 1)
	if !v.writeDone(acctA) {
		t.Fatal("the dropped reload was not asked again after the write")
	}
	if applied, _ := arrive(v.reloadStarted(acctA), []store.Chat{{JID: alice}, {JID: bob, Unread: 1}}); !applied {
		t.Fatal("a fresh list was dropped")
	}
	check("fresh list", 0, 1, 1)

	// a reload starts; a message arrives and is folded in
	seq = v.reloadStarted(acctA)
	side.ApplyMessage(incoming(acctA, bob))
	v.listChanged(acctA)
	v.unread[acctA] = side.Unread()
	if applied, again := arrive(seq, []store.Chat{{JID: alice}, {JID: bob, Unread: 1}}); applied || !again {
		t.Fatalf("list older than a message: applied %v, asked again at once %v", applied, again)
	}
	check("list older than a message", 0, 2, 2)

	// a reload that started before a write and arrives after it ended may have missed it
	seq = v.reloadStarted(acctA)
	v.writeStarted(acctA)
	if v.writeDone(acctA) {
		t.Fatal("asked again although no reload was dropped")
	}
	if applied, again := arrive(seq, []store.Chat{{JID: alice, Unread: 2}, {JID: bob, Unread: 2}}); applied || !again {
		t.Fatalf("list that may predate a finished write: applied %v, asked again %v", applied, again)
	}
	check("list that may predate a finished write", 0, 2, 2)
	// likewise a reload that started while a write was under way
	v.writeStarted(acctA)
	seq = v.reloadStarted(acctA)
	v.writeDone(acctA)
	if applied, again := arrive(seq, []store.Chat{{JID: alice, Unread: 2}, {JID: bob, Unread: 2}}); applied || !again {
		t.Fatalf("list started during a write, back after it: applied %v, asked again %v", applied, again)
	}
	check("list started during a write", 0, 2, 2)

	// other accounts keep their own count
	if applied, _ := v.reloadArrived(acctB, v.reloadStarted(acctB)); !applied {
		t.Fatal("a change in A held back B's list")
	}
}

func TestPresenceBoxKeepsOnlyTheLatestAndNeverDropsIt(t *testing.T) {
	b := newPresenceBox()
	if _, ok := b.take(); ok {
		t.Fatal("an empty box gave a presence")
	}
	for range 1000 { // never blocks, however many wait
		b.put(types.PresenceAvailable)
	}
	b.put(types.PresenceUnavailable)
	select {
	case <-b.wake:
	default:
		t.Fatal("no wake-up for a waiting presence")
	}
	if p, ok := b.take(); !ok || p != types.PresenceUnavailable {
		t.Fatalf("take = %q, %v; want the latest, unavailable", p, ok)
	}
	if _, ok := b.take(); ok {
		t.Fatal("the same presence was taken twice")
	}
}

func TestOlderPagesBelongToOneOpening(t *testing.T) {
	var v view
	g1 := v.openChat(chatRef{acctA, alice})
	if _, _, _, ok := v.nextPage(); ok {
		t.Fatal("an older page was asked for before the chat's first page was shown")
	}
	if !v.firstPageShown(g1) {
		t.Fatal("the first page of the open chat was refused")
	}
	if c, p, g, ok := v.nextPage(); !ok || c != (chatRef{acctA, alice}) || p != 1 || g != g1 {
		t.Fatalf("nextPage = %v, %d, %d, %v", c, p, g, ok)
	}
	if _, p, _, _ := v.nextPage(); p != 2 {
		t.Fatalf("second older page = %d", p)
	}
	g2 := v.openChat(chatRef{acctA, bob})
	if _, _, _, ok := v.nextPage(); ok {
		t.Fatal("the view still shows alice: no page of bob may be asked for")
	}
	if v.current(g1) {
		t.Fatal("alice's answers are still taken after bob opened")
	}
	v.firstPageShown(g2)
	if _, p, _, _ := v.nextPage(); p != 1 {
		t.Fatalf("bob's first older page = %d, want 1", p)
	}
	g3 := v.openChat(chatRef{acctA, alice}) // alice again
	if v.firstPageShown(g1) || v.current(g1) || v.current(g2) {
		t.Fatal("an answer for an earlier opening was taken")
	}
	if !v.firstPageShown(g3) || !v.current(g3) {
		t.Fatal("the current opening was refused")
	}
	if g := v.switchAccount(acctB); v.current(g3) || g == g3 {
		t.Fatal("answers for the chat of the previous account are still taken")
	}
}
