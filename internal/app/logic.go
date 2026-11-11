package app

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"

	"wa-go/internal/media"
	"wa-go/internal/notify"
	"wa-go/internal/store"
	"wa-go/internal/ui"
	"wa-go/internal/whatsapp"
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

// selfOnlyReceipts reports whether a chat read here is told only to the user's own devices
// (read-self): when the account's read receipts are off, or that setting cannot be read.
func selfOnlyReceipts(v string, err error) bool { return err != nil || !receiptsOn(v) }

// typingOn reports whether typing may be shown under the stealth.ghost_typing and
// stealth.appear_offline values: someone appearing offline never shows as typing.
func typingOn(ghost, appearOffline string) bool { return ghost != "true" && appearOffline != "true" }

// listToReload names the account whose chat list (and unread total) evt changed behind the screen's
// back: messages read on another of the user's devices, a chat named. ok is false for other events.
func listToReload(evt any) (acct string, ok bool) {
	switch e := evt.(type) {
	case whatsapp.ReceiptEvent:
		return e.AccountID, e.FromMe && e.Status == store.StatusRead
	case whatsapp.ChatReadEvent:
		return e.AccountID, true
	case whatsapp.ChatNamedEvent:
		return e.AccountID, true
	}
	return "", false
}

// chatNames maps the named chats of a chat list to their names, for toasts and group senders.
func chatNames(chats []store.Chat) map[string]string {
	names := map[string]string{}
	for _, c := range chats {
		if c.Name != "" {
			names[c.JID] = c.Name
		}
	}
	return names
}

// labelOf is jid's name in names, else its phone number.
func labelOf(names map[string]string, jid string) string {
	if n := names[jid]; n != "" {
		return n
	}
	user, server, _ := strings.Cut(jid, "@")
	if server == "s.whatsapp.net" {
		return "+" + user
	}
	return user
}

// dropMissingMedia forgets the download of each message whose cached file is gone (Clear Cache, or
// deleted by hand), so the chat view and the status viewer offer it again. Off the UI thread: it
// touches the disk.
func dropMissingMedia(msgs []store.Message) {
	for i := range msgs {
		if media.Missing(msgs[i].MediaPath) {
			msgs[i].MediaPath = ""
		}
	}
}

// mediaTarget is where an opened media message shows.
type mediaTarget int

const (
	targetViewer   mediaTarget = iota // the media view's photo viewer
	targetPlayer                      // the media view's player (until it comes: the default program)
	targetExternal                    // the default program
)

// targetFor picks where m opens: photos and stickers in the viewer, videos (GIFs too) in the
// player, everything else in its default program.
func targetFor(m store.Message) mediaTarget {
	switch m.Type {
	case "image", "sticker":
		return targetViewer
	case "video":
		return targetPlayer
	}
	return targetExternal
}

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

// windowState is how the user sees Wisp.
type windowState int

const (
	windowForeground windowState = iota // in front: the user is in Wisp (any of its windows)
	windowBackground                    // on screen behind another program, past the grace period
	windowMinimized                     // on the taskbar
	windowHidden                        // closed to the tray
	windowLocked                        // the session is locked
)

// stateOf names the window's state from what Windows reports (visible, iconic), session lock
// notifications (locked), whether Wisp has the foreground (active) and whether the grace period
// after the user switched away still runs.
func stateOf(visible, iconic, locked, active, grace bool) windowState {
	switch {
	case locked:
		return windowLocked
	case !visible:
		return windowHidden
	case iconic:
		return windowMinimized
	case active || grace:
		return windowForeground
	}
	return windowBackground
}

// presenceFor is the presence every account shows: available only while Wisp is in front and the
// user does not appear offline.
func presenceFor(w windowState, appearOffline bool) types.Presence {
	if w == windowForeground && !appearOffline {
		return types.PresenceAvailable
	}
	return types.PresenceUnavailable
}

// graceDelay is how long presence stays available after the user switches to another program.
const graceDelay = 10 * time.Second

// grace delays going unavailable when the user switches away; its one-shot timer exists only while
// pending. UI thread only.
type grace struct {
	after func(d time.Duration, f func()) (stop func() bool) // f runs on the UI thread
	stop  func() bool                                        // nil unless pending
	gen   int                                                // per timer: an expiry queued before cancel is ignored
}

// start begins a grace period unless one runs; expired runs when it ends.
func (g *grace) start(expired func()) {
	if g.stop != nil {
		return
	}
	g.gen++
	gen := g.gen
	g.stop = g.after(graceDelay, func() {
		if gen == g.gen {
			g.stop = nil
			expired()
		}
	})
}

// cancel ends a pending grace period without expiring it.
func (g *grace) cancel() {
	if g.stop != nil {
		g.stop()
		g.stop = nil
		g.gen++
	}
}

func (g *grace) pending() bool { return g.stop != nil }

// presenceCtl decides every account's presence from the window, the session and the appear-offline
// setting, and hands each one to broadcast to send. UI thread only.
type presenceCtl struct {
	window        func() (visible, iconic bool)
	appearOffline func() bool
	send          func(types.Presence)
	active        bool // Wisp has the foreground (WM_ACTIVATEAPP)
	locked        bool // the session is locked
	appLocked     bool // the app lock page covers the window
	grace         grace
	last          presenceState
}

// update broadcasts the presence the state calls for: always when forced (minimised, hidden to the
// tray, locked, a setting change, a reconnect), otherwise only when it changed.
func (p *presenceCtl) update(force bool) {
	visible, iconic := p.window()
	w := stateOf(visible, iconic, p.locked || p.appLocked, p.active, p.grace.pending())
	if pr, send := p.last.next(w, p.appearOffline(), force); send {
		p.send(pr)
	}
}

// activated follows the user switching to Wisp or to another program: back, presence follows at
// once; away, it drops once graceDelay passes unless they come back first.
func (p *presenceCtl) activated(active bool) {
	p.active = active
	if active {
		p.grace.cancel()
		p.update(false)
		return
	}
	p.grace.start(func() { p.update(false) })
}

// sessionLocked: locked, every account goes unavailable at once; unlocked, presence follows the
// window again.
func (p *presenceCtl) sessionLocked(locked bool) {
	p.locked = locked
	p.update(locked)
}

// inFront reports whether the user is looking at the chats: the window is on screen, not minimised,
// in the foreground, and no other page (lock, link) covers the chats.
func inFront(visible, iconic, covered, foreground bool) bool {
	return visible && !iconic && !covered && foreground
}

// modalClass reports whether a window of this class is a dialog that must not outlive a lock: walk's
// dialogs (Settings, New chat, status viewer, pickers, PIN) and Windows' dialogs (file dialogs,
// message boxes). Tooltips and the tray's window are other classes.
func modalClass(class string) bool {
	return class == `\o/ Walk_Dialog_Class \o/` || class == "#32770"
}

// appLock: the lock page covers the window, so the user is not using Wisp; same as a locked session.
func (p *presenceCtl) appLock(locked bool) {
	p.appLocked = locked
	p.update(locked)
}

// toastSettings are the notification settings for a toast: private whatever the setting while the
// app is locked.
func toastSettings(cfg map[string]string, locked bool, accounts int) notify.Settings {
	return notify.Settings{Enabled: true, PrivateMode: locked || cfg[ui.KeyNotifyPrivate] == "true",
		Sound: cfg[ui.KeyNotifySound] == "true", MultiAccount: accounts > 1}
}

// presenceState remembers the last broadcast presence, so window activations do not repeat it.
type presenceState struct{ sent types.Presence }

// next returns the presence for the window's state and the appear-offline setting, and whether to
// broadcast it: always when forced (minimised, hidden to the tray, a setting change, a reconnect),
// otherwise only when it differs from the last one.
func (p *presenceState) next(w windowState, appearOffline, force bool) (types.Presence, bool) {
	want := presenceFor(w, appearOffline)
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
	names    map[string]map[string]string // per account: chat JID to name, from its last applied list
	lists    map[string]*listState        // per account; see reloadArrived
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

// page is what fills the main window.
type page int

const (
	pageChats page = iota // rail, chat list and chat column
	pageLink              // linking a phone
	pageLock              // the lock screen
)

// pageFor picks the page: the lock covers everything, then linking (no account yet, or the user
// is adding one), else the chats.
func pageFor(accounts int, locked, adding bool) page {
	switch {
	case locked:
		return pageLock
	case accounts == 0 || adding:
		return pageLink
	}
	return pageChats
}

// pairingRuns reports whether the link page should be pairing: it is the page, and the window is
// on screen (not hidden to the tray, not minimised).
func pairingRuns(pg page, visible, iconic bool) bool { return pg == pageLink && visible && !iconic }

// linkCancellable: Cancel returns to the chats, so it needs an account and an add in progress.
func linkCancellable(adding bool, accounts int) bool { return adding && accounts > 0 }

// logoutAll logs every account out, trying all of them even when one fails (offline), and returns
// the ones that failed.
func logoutAll(ctx context.Context, accounts []string, logout func(context.Context, string) error) (failed []string) {
	for _, id := range accounts {
		if err := logout(ctx, id); err != nil {
			log.Printf("app: logout %s: %v", id, err)
			failed = append(failed, id)
		}
	}
	return failed
}
