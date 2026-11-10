package app

import (
	"sync"

	"go.mau.fi/whatsmeow/types"

	"wa-go/internal/store"
)

// The window-free rules of the wiring, tested on their own. UI thread only, except presenceBox.

const statusBroadcast = "status@broadcast" // stories: never a chat

// chatRef names one chat of one account; the zero value is no chat.
type chatRef struct{ acct, jid string }

// msgRoute is what a just-stored message (a whatsapp.MessageEvent) changes on screen.
type msgRoute struct {
	sidebar  bool // fold it into the sidebar: its account is the active one
	upsert   bool // add it to the open chat
	stories  bool // refresh the stories strip instead: a status update of the active account
	unread   bool // it counts as unread for its account
	markRead bool // the user is looking at its chat: mark it read now
	notify   bool // toast it (notify.Format still filters by type and privacy)
}

// routeMessage decides what m changes, given the active account, the open chat, whether the window
// is in the foreground and whether toasts are on. A message only ever touches its own account's
// sidebar and chat.
func routeMessage(m store.Message, active string, open chatRef, foreground, notifyOn bool) msgRoute {
	if m.ChatJID == statusBroadcast {
		return msgRoute{stories: m.AccountID == active}
	}
	isOpen := open == chatRef{m.AccountID, m.ChatJID}
	seen := isOpen && foreground
	in := !m.FromMe && m.Status != store.StatusRead
	return msgRoute{
		sidebar:  m.AccountID == active,
		upsert:   isOpen,
		unread:   in && !seen,
		markRead: in && seen,
		notify:   notifyOn && !m.FromMe && !seen,
	}
}

// receiptsOn reports whether read receipts may be sent under an account's privacy.read_receipts value.
func receiptsOn(v string) bool { return v != "none" }

// typingOn reports whether typing may be shown under the stealth.ghost_typing value.
func typingOn(ghost string) bool { return ghost != "true" }

// unreadIncoming returns the messages of msgs a read receipt is due for.
func unreadIncoming(msgs []store.Message) []store.Message {
	var out []store.Message
	for _, m := range msgs {
		if !m.FromMe && m.Status != store.StatusRead {
			out = append(out, m)
		}
	}
	return out
}

// presenceFor is the presence every account shows: available only while the window is visible and
// the user does not appear offline.
func presenceFor(visible, appearOffline bool) types.Presence {
	if visible && !appearOffline {
		return types.PresenceAvailable
	}
	return types.PresenceUnavailable
}

// presenceState remembers the last broadcast presence, so window activations do not repeat it.
type presenceState struct{ sent types.Presence }

// next returns the presence for the window's visibility and the appear-offline setting, and whether
// to broadcast it: always when forced (hidden to the tray, exit, a setting change, a reconnect),
// otherwise only when it differs from the last one.
func (p *presenceState) next(visible, appearOffline, force bool) (types.Presence, bool) {
	want := presenceFor(visible, appearOffline)
	if !force && want == p.sent {
		return want, false
	}
	p.sent = want
	return want, true
}

// gifKey is the GIPHY API key to search with: the one in Settings, else the one built in.
func gifKey(setting, builtin string) string {
	if setting != "" {
		return setting
	}
	return builtin
}

// totalUnread sums the unread counts of the listed accounts, for the tray badge.
func totalUnread(unread map[string]int, accounts []string) int {
	n := 0
	for _, a := range accounts {
		n += unread[a]
	}
	return n
}

// mediaKey names one message's download; message IDs repeat across accounts (a group both numbers
// are in) and chats.
type mediaKey struct{ acct, chat, id string }

// inflight holds the downloads under way, shared by the chat view and the status viewer.
type inflight map[mediaKey]bool

// start reports whether k may start: false while it is already running.
func (f inflight) start(k mediaKey) bool {
	if f[k] {
		return false
	}
	f[k] = true
	return true
}

func (f inflight) done(k mediaKey) { delete(f, k) }

// view is what the window shows: the active account, the open chat and its paging, and every
// account's unread total.
type view struct {
	active   string
	open     chatRef         // zero while no chat is open
	gen      int             // per opening of a chat: answers for an earlier opening are dropped
	page     int             // the open chat's last page asked for
	loaded   bool            // the chat view shows open: its first page arrived
	pending  []store.Message // messages of open that arrived before its first page
	openNext chatRef         // open this chat once its account's chats load (a toast of another account)
	unread   map[string]int
	lists    map[string]*listState // per account; see reloadArrived
}

// switchAccount makes acct active; the chat pane empties. It returns the new opening (of no chat).
func (v *view) switchAccount(acct string) int {
	v.active = acct
	return v.openChat(chatRef{})
}

// openChat makes c the open chat (zero: none), not yet loaded, and returns the generation that names
// this opening.
func (v *view) openChat(c chatRef) int {
	v.gen++
	v.open, v.page, v.loaded, v.pending = c, 0, false, nil
	return v.gen
}

// current reports whether gen is still the open chat's opening.
func (v *view) current(gen int) bool { return gen == v.gen }

// firstPageShown records that the first page of opening gen is shown; false (nothing changes) for an
// earlier opening.
func (v *view) firstPageShown(gen int) bool {
	if !v.current(gen) {
		return false
	}
	v.loaded = true
	return true
}

// nextPage returns the open chat, the next older page to load and its opening; ok is false while
// no chat is shown, or while the chat view still shows the chat before (its first page is loading).
func (v *view) nextPage() (c chatRef, page, gen int, ok bool) {
	if v.open.jid == "" || !v.loaded {
		return chatRef{}, 0, 0, false
	}
	v.page++
	return v.open, v.page, v.gen, true
}

// listState tracks what a reloaded chat list of one account may have missed.
type listState struct {
	seq     int  // changes made on screen, and store writes started or ended
	writing int  // store writes under way (mark read) that a reload may not see yet
	again   bool // a reload was dropped while writing: ask again once the writes are done
}

func (v *view) list(acct string) *listState {
	if v.lists == nil {
		v.lists = map[string]*listState{}
	}
	if v.lists[acct] == nil {
		v.lists[acct] = &listState{}
	}
	return v.lists[acct]
}

// listChanged records an on-screen change to acct's chat list or unread total (a message folded in).
func (v *view) listChanged(acct string) { v.list(acct).seq++ }

// reloadStarted returns the mark a reload of acct's chat list hands back to reloadArrived.
func (v *view) reloadStarted(acct string) int { return v.list(acct).seq }

// writeStarted records a store write (mark read) of acct whose effect is already on screen.
func (v *view) writeStarted(acct string) {
	l := v.list(acct)
	l.writing++
	l.seq++
}

// writeDone records the end of a store write of acct and reports whether to reload its chat list now
// (a reload was dropped while it ran).
func (v *view) writeDone(acct string) bool {
	l := v.list(acct)
	l.writing--
	l.seq++ // a reload under way may have read the store before the write
	if l.writing > 0 || !l.again {
		return false
	}
	l.again = false
	return true
}

// reloadArrived decides on a chat list of acct read from the store since reloadStarted gave seq: it
// is applied only when nothing changed on screen since and no write it may have missed is under way,
// so a stale list never undoes newer changes. A dropped list is asked for again: at once (again), or
// after the writes under way (writeDone).
func (v *view) reloadArrived(acct string, seq int) (apply, again bool) {
	l := v.list(acct)
	switch {
	case l.writing > 0:
		l.again = true
		return false, false
	case l.seq != seq:
		return false, true
	}
	return true, false
}

// presenceBox holds the presence waiting to be broadcast: a newer one replaces it, none is ever
// dropped, and wake (one slot) says there is one.
type presenceBox struct {
	mu   sync.Mutex
	p    types.Presence // "" when none waits
	wake chan struct{}
}

func newPresenceBox() *presenceBox { return &presenceBox{wake: make(chan struct{}, 1)} }

func (b *presenceBox) put(p types.Presence) {
	b.mu.Lock()
	b.p = p
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// take returns the waiting presence, once.
func (b *presenceBox) take() (types.Presence, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.p
	b.p = ""
	return p, p != ""
}

// takeOpenNext returns, once, the chat queued to open in acct.
func (v *view) takeOpenNext(acct string) (jid string, ok bool) {
	if v.openNext.acct != acct || v.openNext.jid == "" {
		return "", false
	}
	jid = v.openNext.jid
	v.openNext = chatRef{}
	return jid, true
}
