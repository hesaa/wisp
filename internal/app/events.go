package app

import (
	"log"
	"slices"
	"strings"
	"time"

	"wa-go/internal/notify"
	"wa-go/internal/store"
	"wa-go/internal/ui"
	"wa-go/internal/whatsapp"
)

// listen receives the manager's events on whatsmeow's goroutines and hands them to the UI thread;
// it never blocks and never touches a widget itself.
func (a *App) listen(evt any) {
	a.mw.Synchronize(func() { a.handle(evt) })
}

// handle applies one manager event. UI thread.
func (a *App) handle(evt any) {
	if acct, ok := listToReload(evt); ok { // any account: the rail and tray totals follow
		a.reloadList(acct)
	}
	switch e := evt.(type) {
	case whatsapp.MessageEvent:
		a.onMessage(e.Message)
	case whatsapp.ReceiptEvent:
		a.reload(chatRef{e.AccountID, e.ChatJID}, e.IDs)
	case whatsapp.RevisedEvent:
		a.reload(chatRef{e.AccountID, e.ChatJID}, []string{e.ID})
	case whatsapp.ChatNamedEvent:
		a.chat.Rename(e.AccountID, e.ChatJID, e.Name)
	case whatsapp.PeopleEvent:
		a.loadPeople(e.AccountID)
	case whatsapp.PushNameEvent:
		a.sendPresence(true) // the first one after a pairing failed without the name
	case whatsapp.HistorySyncEvent:
		a.loadChats(e.AccountID)
	case whatsapp.ConnectionEvent:
		a.onConnection(e)
	case whatsapp.ErrorEvent:
		logErr("+"+e.AccountID, e.Err)
	}
}

// onMessage folds a just-stored message into the screen (see routeMessage).
func (a *App) onMessage(m store.Message) {
	r := routeMessage(m, a.v.active, a.v.open, a.foreground(), a.cfg[ui.KeyNotify] == "true")
	if r.stories {
		a.loadStories(m.AccountID)
		return
	}
	if r.markRead {
		a.markMessageRead(m)
		m.Status = store.StatusRead
	}
	if r.upsert {
		if a.v.loaded {
			a.chat.Upsert(m)
		} else {
			a.v.pending = append(a.v.pending, m) // its chat's first page is still loading
		}
	}
	if r.sidebar && a.side.Model().ApplyMessage(m) {
		a.v.unread[m.AccountID] = a.side.Model().Unread()
		a.v.listChanged(m.AccountID)
	} else if r.unread {
		a.reloadList(m.AccountID) // another account: its total comes from its stored list
	}
	a.showUnread()
	if r.notify {
		a.toast(m)
	}
}

// reload re-reads changed messages of c (receipts, edits, deletions) into the chat view when c is open.
func (a *App) reload(c chatRef, ids []string) {
	if a.v.open != c {
		return
	}
	go func() {
		var msgs []store.Message
		for _, id := range ids {
			m, ok, err := a.db.GetMessage(c.acct, c.jid, id)
			logErr("reload message", err)
			if ok {
				msgs = append(msgs, m)
			}
		}
		a.mw.Synchronize(func() {
			if a.v.open != c {
				return
			}
			for _, m := range msgs {
				if a.v.loaded {
					a.chat.Upsert(m)
				} else {
					a.v.pending = append(a.v.pending, m)
				}
			}
		})
	}()
}

func (a *App) onConnection(e whatsapp.ConnectionEvent) {
	switch e.State {
	case whatsapp.StateConnected:
		a.refreshAccounts() // it may have connected before Start returned
		a.rail.Model().SetConnected(e.AccountID, true)
		a.sendPresence(true) // a new connection starts unavailable
	case whatsapp.StatePaired:
		a.refreshAccounts()
		a.rail.Model().SetActive(e.AccountID)
		a.adding = false
		a.showPage()
	case whatsapp.StateLoggedOut:
		log.Printf("app: +%s logged out (%s)", e.AccountID, e.Detail)
		a.refreshAccounts() // the rail drops it and picks another active account
	default:
		a.rail.Model().SetConnected(e.AccountID, false)
		log.Printf("app: +%s %s (%s)", e.AccountID, e.State, e.Detail)
	}
}

// refreshAccounts shows the manager's accounts on the rail and counts the unread messages of new ones.
func (a *App) refreshAccounts() {
	ids := a.mgr.Accounts()
	a.rail.Model().SetAccounts(ids) // may switch the active account
	for id := range a.v.unread {
		if !slices.Contains(ids, id) {
			delete(a.v.unread, id)
			delete(a.v.names, id)
		}
	}
	for _, id := range ids {
		if _, known := a.v.unread[id]; !known {
			a.v.unread[id] = 0
			if id != a.v.active { // the active one was loaded by the switch
				a.loadChats(id)
			}
		}
	}
	a.showUnread()
	hint := "Choose a chat, or start a new one."
	if len(ids) == 0 {
		hint = "Link your phone with the + button on the left to start."
	}
	a.empty.SetHint(hint)
	a.showPage()
}

// accountChanged switches the window to acct ("" when no account is left): the chat pane empties
// and acct's chats and stories load.
func (a *App) accountChanged(acct string) {
	a.closeMedia()
	a.bar.Reset() // before the switch: it may still tell the chat being left that typing stopped
	a.v.switchAccount(acct)
	a.showChatPane(false)
	a.side.Model().SetChats(acct, nil)
	a.stories = nil
	a.side.SetStories(nil)
	if acct != "" {
		a.loadChats(acct)
	}
}

// loadChats reloads acct's chat list, and its stories when it is the active account.
func (a *App) loadChats(acct string) {
	if acct == a.v.active {
		a.loadStories(acct)
	}
	a.reloadList(acct)
}

// reloadList reads acct's chat list off the UI thread: its unread total, and the sidebar when it is
// the active account. A list that may be older than what the screen shows is dropped and read again
// (see view.reloadArrived).
func (a *App) reloadList(acct string) {
	seq := a.v.reloadStarted(acct)
	go func() {
		chats, err := a.db.ListChats(acct)
		people, _ := a.db.People(acct)
		a.mw.Synchronize(func() {
			if err != nil {
				logErr("chats of +"+acct, err)
				return
			}
			if !slices.Contains(a.rail.Model().Accounts(), acct) {
				return // logged out meanwhile
			}
			apply, again := a.v.reloadArrived(acct, seq)
			if again {
				a.reloadList(acct)
			}
			if !apply {
				return
			}
			n := 0
			for _, c := range chats {
				n += c.Unread
			}
			a.v.unread[acct] = n
			a.v.names[acct] = chatNames(chats)
			d := ui.NewDirectory(people)
			if a.v.dirs == nil {
				a.v.dirs = make(map[string]*ui.Directory)
			}
			a.v.dirs[acct] = d
			if acct == a.v.active {
				a.side.SetDirectory(d)
				a.chat.SetDirectory(d)
				a.side.Model().SetChats(acct, chats)
				if jid, ok := a.v.takeOpenNext(acct); ok {
					a.openChat(acct, jid, "")
				}
			}
			a.showUnread()
		})
	}()
}

// loadPeople refreshes acct's directory index when a PeopleEvent arrives.
func (a *App) loadPeople(acct string) {
	go func() {
		people, err := a.db.People(acct)
		a.mw.Synchronize(func() {
			if err != nil {
				logErr("people of +"+acct, err)
				return
			}
			d := ui.NewDirectory(people)
			if a.v.dirs == nil {
				a.v.dirs = make(map[string]*ui.Directory)
			}
			a.v.dirs[acct] = d
			if acct == a.v.active {
				a.side.SetDirectory(d)
				a.chat.SetDirectory(d)
			}
		})
	}()
}

// loadStories refreshes the stories strip of acct, the active account, from the last 24 hours.
func (a *App) loadStories(acct string) {
	go func() {
		st, err := a.db.RecentStories(acct, time.Now().Add(-24*time.Hour).Unix())
		a.mw.Synchronize(func() {
			logErr("stories of +"+acct, err)
			if err == nil && acct == a.v.active {
				a.stories = st
				a.side.SetStories(st)
			}
		})
	}()
}

// showUnread puts every account's unread total on the rail and their sum on the tray icon.
func (a *App) showUnread() {
	ids := a.rail.Model().Accounts()
	for _, id := range ids {
		a.rail.Model().SetUnread(id, a.v.unread[id])
	}
	if a.tray != nil {
		logErr("tray badge", a.tray.SetUnread(totalUnread(a.v.unread, ids)))
	}
}

// toast alerts the user to m through the tray, honouring the notification settings.
func (a *App) toast(m store.Message) {
	in := notify.Input{AccountID: m.AccountID, AccountLabel: "+" + m.AccountID, ChatJID: m.ChatJID,
		SenderName: a.label(m.AccountID, m.SenderJID), ChatName: a.label(m.AccountID, m.ChatJID), IsGroup: isGroup(m.ChatJID),
		Type: m.Type, Body: m.Body, FromMe: m.FromMe}
	if m.Type == whatsapp.TypeDocument {
		in.FileName = m.Body // a document's body is its caption, else its file name
	}
	s := toastSettings(a.cfg, a.locked, len(a.rail.Model().Accounts()))
	if a.tray != nil {
		logErr("toast", a.tray.Notify(in, s))
	}
}

// toastClicked brings the window back on the toast's chat, switching account first if needed.
func (a *App) toastClicked(acct, jid string) {
	a.restore()
	if a.locked {
		return // the chat opens after unlocking, by hand
	}
	if acct == a.v.active {
		a.openChat(acct, jid, "")
		return
	}
	a.v.openNext = chatRef{acct, jid}
	a.rail.Model().SetActive(acct) // its chats load, then the chat opens
}

// chatName is jid's name in acct's chat list (any account) by the name rule.
func (a *App) chatName(acct, jid string) string {
	var stored string
	if a.v.names != nil && a.v.names[acct] != nil {
		stored = a.v.names[acct][jid]
	}
	return a.v.dirs[acct].Name(jid, stored)
}

// label is jid's name in acct's chat list, else its phone number or "Unknown".
func (a *App) label(acct, jid string) string { return labelOf(a.v.names[acct], jid, a.v.dirs[acct]) }

// chatUnread is the open account's unread count of jid.
func (a *App) chatUnread(jid string) int {
	for _, c := range a.side.Model().Chats() {
		if c.JID == jid {
			return c.Unread
		}
	}
	return 0
}

func isGroup(jid string) bool { return strings.HasSuffix(jid, "@g.us") }
