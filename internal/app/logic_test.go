package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/store"
	"wa-go/internal/ui"
	"wa-go/internal/whatsapp"
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
	for _, c := range []struct {
		ghost, offline string
		want           bool
	}{{"false", "false", true}, {"", "", true}, {"true", "false", false}, {"false", "true", false}, {"true", "true", false}} {
		if typingOn(c.ghost, c.offline) != c.want {
			t.Errorf("typingOn(ghost %q, appear offline %q) = %v", c.ghost, c.offline, !c.want)
		}
	}
	// read here is always synced to the user's own devices; senders hear of it only with receipts on
	for _, c := range []struct {
		v    string
		err  error
		self bool
	}{{"all", nil, false}, {"contacts", nil, false}, {"none", nil, true}, {"all", errors.New("db"), true}} {
		if selfOnlyReceipts(c.v, c.err) != c.self {
			t.Errorf("selfOnlyReceipts(%q, %v) = %v", c.v, c.err, !c.self)
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

func TestPresenceForEveryWindowState(t *testing.T) {
	for _, w := range []windowState{windowForeground, windowBackground, windowMinimized, windowHidden, windowLocked} {
		for _, appearOffline := range []bool{false, true} {
			want := types.PresenceUnavailable
			if w == windowForeground && !appearOffline {
				want = types.PresenceAvailable // only in front, and only when not appearing offline
			}
			if got := presenceFor(w, appearOffline); got != want {
				t.Errorf("presenceFor(%v, appear offline %v) = %q, want %q", w, appearOffline, got, want)
			}
		}
	}
}

func TestPresenceDecisions(t *testing.T) {
	type step struct {
		window        windowState
		appearOffline bool
		force         bool
		want          types.Presence
		send          bool
	}
	on, off := types.PresenceAvailable, types.PresenceUnavailable
	for name, steps := range map[string][]step{
		"startup, activations, minimise, restore, tray, restore": {
			{windowForeground, false, false, on, true},
			{windowForeground, false, false, on, false}, // another activation: nothing new
			{windowMinimized, false, true, off, true},   // minimised: at once
			{windowMinimized, false, false, off, false}, // still minimised (an activation)
			{windowForeground, false, false, on, true},  // restored
			{windowHidden, false, true, off, true},      // closed to the tray: at once
			{windowForeground, false, false, on, true},  // restored from the tray
			{windowForeground, false, true, on, true},   // reconnected: said again
			{windowBackground, false, false, off, true}, // behind another program, grace over
			{windowBackground, false, true, off, true},  // reconnected there: unavailable, not available
			{windowLocked, false, true, off, true},      // locked
			{windowForeground, false, false, on, true},  // unlocked, in front
		},
		"appear offline wins in every state": {
			{windowForeground, true, false, off, true}, // startup
			{windowForeground, true, false, off, false},
			{windowMinimized, true, false, off, false},
			{windowForeground, true, false, off, false},
			{windowHidden, true, true, off, true},
			{windowForeground, true, false, off, false}, // restored: still offline, already said
			{windowBackground, true, true, off, true},   // reconnected behind another program
			{windowLocked, true, true, off, true},
			{windowForeground, false, true, on, true}, // setting turned off
		},
	} {
		var p presenceState
		for i, s := range steps {
			got, send := p.next(s.window, s.appearOffline, s.force)
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

func TestStateOf(t *testing.T) {
	for _, c := range []struct {
		visible, iconic, locked, active, grace bool
		want                                   windowState
	}{
		{true, false, false, true, false, windowForeground},
		{true, false, false, false, true, windowForeground}, // just left: the grace period runs
		{true, false, false, false, false, windowBackground},
		{true, true, false, true, false, windowMinimized},
		{false, false, false, true, false, windowHidden},
		{true, false, true, true, true, windowLocked}, // a locked session wins over everything
	} {
		if got := stateOf(c.visible, c.iconic, c.locked, c.active, c.grace); got != c.want {
			t.Errorf("stateOf(%+v) = %v, want %v", c, got, c.want)
		}
	}
}

// presenceRig is a presenceCtl over a fake window and fake timers, recording what it broadcasts.
type presenceRig struct {
	presenceCtl
	visible, iconic, offline bool
	sent                     []types.Presence
	timers                   []*rigTimer
}

type rigTimer struct {
	f       func()
	stopped bool
}

func newPresenceRig() *presenceRig {
	r := &presenceRig{visible: true}
	r.window = func() (bool, bool) { return r.visible, r.iconic }
	r.appearOffline = func() bool { return r.offline }
	r.send = func(p types.Presence) { r.sent = append(r.sent, p) }
	r.grace.after = func(d time.Duration, f func()) func() bool {
		if d != graceDelay {
			panic("unexpected delay")
		}
		t := &rigTimer{f: f}
		r.timers = append(r.timers, t)
		return func() bool { was := !t.stopped; t.stopped = true; return was }
	}
	return r
}

// live counts timers neither fired nor stopped.
func (r *presenceRig) live() int {
	n := 0
	for _, t := range r.timers {
		if !t.stopped {
			n++
		}
	}
	return n
}

// expire fires the newest timer as its expiry would.
func (r *presenceRig) expire() {
	t := r.timers[len(r.timers)-1]
	t.stopped = true
	t.f()
}

func (r *presenceRig) expect(t *testing.T, step string, want ...types.Presence) {
	t.Helper()
	if len(r.sent) != len(want) {
		t.Fatalf("%s: sent %v, want %v", step, r.sent, want)
	}
	for i := range want {
		if r.sent[i] != want[i] {
			t.Fatalf("%s: sent %v, want %v", step, r.sent, want)
		}
	}
	r.sent = nil
}

func TestPresenceFollowsTheForeground(t *testing.T) {
	on, off := types.PresenceAvailable, types.PresenceUnavailable
	r := newPresenceRig()
	r.activated(true)
	r.expect(t, "in front at startup", on)

	r.activated(false)
	r.expect(t, "just switched away: the grace period runs")
	if r.live() != 1 {
		t.Fatalf("%d grace timers, want 1", r.live())
	}
	r.activated(false) // a second deactivation does not restart it
	if len(r.timers) != 1 {
		t.Fatalf("grace restarted: %d timers", len(r.timers))
	}
	r.expire()
	r.expect(t, "grace over in the background", off)
	if r.live() != 0 {
		t.Fatal("a timer outlived its grace period")
	}

	r.activated(true)
	r.expect(t, "back in front", on)
	r.activated(false)
	r.activated(true)
	r.expect(t, "back within the grace period: never unavailable")
	if r.live() != 0 {
		t.Fatal("the cancelled grace timer is still live")
	}
	r.timers[len(r.timers)-1].f() // an expiry queued before the cancel
	r.expect(t, "a stale expiry")

	r.update(true)
	r.expect(t, "reconnected in front", on)
	r.activated(false)
	r.expire()
	r.expect(t, "background again", off)
	r.update(true)
	r.expect(t, "reconnected in the background: what the rule says, not available", off)
}

func TestPresenceLockMinimiseAndAppearOffline(t *testing.T) {
	on, off := types.PresenceAvailable, types.PresenceUnavailable
	r := newPresenceRig()
	r.activated(true)
	r.expect(t, "startup", on)

	r.activated(false) // locking takes the foreground too
	r.sessionLocked(true)
	r.expect(t, "locked: at once, grace or not", off)
	r.expire()
	r.expect(t, "grace over while locked")
	r.sessionLocked(false)
	r.expect(t, "unlocked, another program in front")
	r.activated(true)
	r.expect(t, "unlocked and in front", on)
	r.activated(true)
	r.sessionLocked(true)
	r.sessionLocked(false)
	r.expect(t, "locked and unlocked while in front", off, on)

	r.iconic = true
	r.update(true)
	r.expect(t, "minimised: at once", off)
	r.iconic = false
	r.update(false)
	r.expect(t, "restored", on)
	r.visible = false
	r.update(true)
	r.expect(t, "hidden to the tray: at once", off)
	r.visible = true

	r.offline = true
	r.update(true)
	r.expect(t, "appear offline turned on", off)
	r.activated(false)
	r.expire()
	r.activated(true)
	r.sessionLocked(true)
	r.sessionLocked(false)
	r.expect(t, "appearing offline, nothing ever says available", off)
}

func TestListToReload(t *testing.T) {
	for _, c := range []struct {
		evt  any
		acct string
		ok   bool
	}{
		{whatsapp.ReceiptEvent{AccountID: acctB, Status: store.StatusRead, FromMe: true}, acctB, true}, // read-self on the phone
		{whatsapp.ReceiptEvent{AccountID: acctB, Status: store.StatusRead}, acctB, false},              // a contact read mine
		{whatsapp.ReceiptEvent{AccountID: acctB, Status: store.StatusDelivered, FromMe: true}, acctB, false},
		{whatsapp.ChatReadEvent{AccountID: acctB, ChatJID: alice}, acctB, true},
		{whatsapp.ChatNamedEvent{AccountID: acctA, ChatJID: "1@g.us", Name: "Team"}, acctA, true},
		{whatsapp.HistorySyncEvent{AccountID: acctA}, "", false}, // loadChats handles it
	} {
		if acct, ok := listToReload(c.evt); ok != c.ok || (ok && acct != c.acct) {
			t.Errorf("listToReload(%#v) = %q, %v", c.evt, acct, ok)
		}
	}
}

func TestToastLabelsComeFromEveryAccountsList(t *testing.T) {
	// account B is not active: its names come from its own applied list, not the sidebar
	v := view{active: acctA, names: map[string]map[string]string{
		acctB: chatNames([]store.Chat{{JID: alice, Name: "Alice"}, {JID: "120363@g.us", Name: "Family"}, {JID: bob}}),
	}}
	for jid, want := range map[string]string{alice: "Alice", "120363@g.us": "Family", bob: "+6282222222222"} {
		if got := labelOf(v.names[acctB], jid); got != want {
			t.Errorf("label of %s in B = %q, want %q", jid, got, want)
		}
	}
	if got := labelOf(v.names[acctA], alice); got != "+6281111111111" {
		t.Errorf("A has no name for alice, got %q", got)
	}
}

func TestDropMissingMedia(t *testing.T) {
	dir := t.TempDir()
	kept := filepath.Join(dir, "kept.jpg")
	if err := os.WriteFile(kept, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	msgs := []store.Message{{ID: "a", MediaPath: kept}, {ID: "b", MediaPath: filepath.Join(dir, "cleared.jpg")}, {ID: "c"}}
	dropMissingMedia(msgs)
	if msgs[0].MediaPath != kept || msgs[1].MediaPath != "" || msgs[2].MediaPath != "" {
		t.Fatalf("after: %+v", msgs)
	}
}

func TestTargetFor(t *testing.T) {
	cases := map[string]mediaTarget{"image": targetViewer, "sticker": targetViewer, "video": targetPlayer,
		"audio": targetExternal, "document": targetExternal}
	for typ, want := range cases {
		if got := targetFor(store.Message{Type: typ}); got != want {
			t.Errorf("%s: %v, want %v", typ, got, want)
		}
	}
	meta, err := proto.Marshal(&waE2E.VideoMessage{GifPlayback: proto.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if got := targetFor(store.Message{Type: "video", MediaMeta: meta}); got != targetPlayer {
		t.Errorf("gif: %v, want the player", got)
	}
}

func TestPageFor(t *testing.T) {
	cases := []struct {
		accounts int
		loaded   bool
		locked   bool
		adding   bool
		want     page
	}{
		{0, true, false, false, pageLink}, {1, true, false, false, pageChats}, {1, true, false, true, pageLink},
		{0, true, true, false, pageLock}, {2, true, true, true, pageLock}, {2, true, false, false, pageChats},
		// start-up: the accounts are not loaded yet, so none is not "no account" (no link page, no pairing)
		{0, false, false, false, pageNone}, {0, false, false, true, pageNone}, {0, false, true, false, pageLock},
	}
	for _, c := range cases {
		if got := pageFor(c.accounts, c.loaded, c.locked, c.adding); got != c.want {
			t.Errorf("pageFor(%d, %v, %v, %v) = %v, want %v", c.accounts, c.loaded, c.locked, c.adding, got, c.want)
		}
		if c.want == pageNone && pairingRuns(c.want, true, false) {
			t.Errorf("pairing runs before the accounts are loaded")
		}
	}
}

// Two accounts, one logs out: the chats stay; the last one logs out: the link page shows.
func TestPageAfterLogouts(t *testing.T) {
	if got := pageFor(1, true, false, false); got != pageChats {
		t.Errorf("one account left: %v, want pageChats", got)
	}
	if got := pageFor(0, true, false, false); got != pageLink {
		t.Errorf("none left: %v, want pageLink", got)
	}
}

// The media view closes (nothing plays) unless the chats are in front of a window on screen.
func TestMediaStays(t *testing.T) {
	cases := []struct {
		pg              page
		visible, iconic bool
		want            bool
	}{
		{pageChats, true, false, true}, {pageChats, false, false, false}, {pageChats, true, true, false},
		{pageLink, true, false, false}, {pageLock, true, false, false}, {pageNone, true, false, false},
	}
	for _, c := range cases {
		if got := mediaStays(c.pg, c.visible, c.iconic); got != c.want {
			t.Errorf("mediaStays(%v, %v, %v) = %v, want %v", c.pg, c.visible, c.iconic, got, c.want)
		}
	}
}

func TestPairingRuns(t *testing.T) {
	cases := []struct {
		pg              page
		visible, iconic bool
		want            bool
	}{
		{pageLink, true, false, true}, {pageLink, false, false, false}, {pageLink, true, true, false},
		{pageChats, true, false, false}, {pageLock, true, false, false},
	}
	for _, c := range cases {
		if got := pairingRuns(c.pg, c.visible, c.iconic); got != c.want {
			t.Errorf("pairingRuns(%v, %v, %v) = %v, want %v", c.pg, c.visible, c.iconic, got, c.want)
		}
	}
}

func TestLinkCancellable(t *testing.T) {
	if !linkCancellable(true, 1) || linkCancellable(true, 0) || linkCancellable(false, 2) {
		t.Error("Cancel needs adding and at least one account")
	}
}

func TestInFront(t *testing.T) {
	if !inFront(true, false, false, true) {
		t.Error("visible, in front, unlocked: want in front")
	}
	for _, c := range [][4]bool{{false, false, false, true}, {true, true, false, true}, {true, false, true, true}, {true, false, false, false}} {
		if inFront(c[0], c[1], c[2], c[3]) {
			t.Errorf("inFront%v = true, want false", c)
		}
	}
}

func TestModalClass(t *testing.T) {
	for class, want := range map[string]bool{`\o/ Walk_Dialog_Class \o/`: true, "#32770": true, "tooltips_class32": false,
		`\o/ Walk_MainWindow_Class \o/`: false, "": false} {
		if got := modalClass(class); got != want {
			t.Errorf("modalClass(%q) = %v, want %v", class, got, want)
		}
	}
}

func TestToastSettingsPrivateWhileLocked(t *testing.T) {
	cfg := map[string]string{ui.KeyNotifyPrivate: "false", ui.KeyNotifySound: "true"}
	if s := toastSettings(cfg, true, 2); !s.PrivateMode || !s.Sound || !s.MultiAccount || !s.Enabled {
		t.Errorf("locked: %+v, want private, sound, multi-account", s)
	}
	if s := toastSettings(cfg, false, 1); s.PrivateMode || !s.Sound || s.MultiAccount {
		t.Errorf("unlocked: %+v, want not private, sound, single account", s)
	}
	if s := toastSettings(map[string]string{ui.KeyNotifyPrivate: "true"}, false, 1); !s.PrivateMode {
		t.Errorf("unlocked, private setting: %+v, want private", s)
	}
}

func TestPresenceUnavailableWhileAppLocked(t *testing.T) {
	on, off := types.PresenceAvailable, types.PresenceUnavailable
	r := newPresenceRig()
	r.activated(true)
	r.expect(t, "startup", on)
	r.appLock(true)
	r.expect(t, "app locked: at once", off)
	r.sessionLocked(true)
	r.sessionLocked(false)
	r.expect(t, "session lock forces a send; its unlock keeps the app lock", off)
	r.appLock(false)
	r.expect(t, "app unlocked", on)
}

func TestLogoutAllReportsFailuresAndTriesEvery(t *testing.T) {
	var called []string
	failed := logoutAll(context.Background(), []string{"a", "b", "c"}, func(_ context.Context, id string) error {
		called = append(called, id)
		if id != "b" {
			return errors.New("offline")
		}
		return nil
	})
	if !slices.Equal(failed, []string{"a", "c"}) || !slices.Equal(called, []string{"a", "b", "c"}) {
		t.Errorf("failed %v, called %v", failed, called)
	}
}
