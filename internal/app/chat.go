package app

import (
	"context"
	"log"
	"os"
	"time"

	"wa-go/internal/store"
	"wa-go/internal/ui"
	"wa-go/internal/whatsapp"
)

// gifHTTP downloads picked GIFs, through the proxy in Settings.
var gifHTTP = newGIFClient(ui.GIFTransport, time.Minute)

// showChatPane shows the chat and the input bar, or the empty state.
func (a *App) showChatPane(open bool) {
	a.empty.SetVisible(!open)
	a.chat.SetVisible(open)
	a.bar.SetVisible(open)
}

// openChat opens acct's chat jid (named name, or as the chat list names it): its newest page loads
// off the UI thread, and it is marked read. Opening the open chat again only reads it and focuses
// the input box, so a draft survives.
func (a *App) openChat(acct, jid, name string) {
	c := chatRef{acct, jid}
	if acct == "" || jid == "" || acct != a.v.active {
		return
	}
	a.side.Model().Select(jid)
	if c == a.v.open {
		if a.chatUnread(jid) > 0 {
			a.markChatRead(c)
		}
		a.bar.Focus()
		return
	}
	if name == "" {
		name = a.chatName(acct, jid)
	}
	a.bar.Reset() // before the switch: it may still tell the chat being left that typing stopped
	a.loadChat(c, name)
	a.showChatPane(true)
	a.bar.Focus()
}

// loadChat shows c in the chat view from its newest page, read off the UI thread once disappeared
// messages are gone, and marks it read. The input bar is left alone.
func (a *App) loadChat(c chatRef, name string) {
	gen := a.v.openChat(c)
	go func() {
		purged := a.purgeExpired()
		page, err := a.db.GetChatMessages(c.acct, c.jid, 0)
		dropMissingMedia(page)
		a.mw.Synchronize(func() {
			if purged {
				for _, acct := range a.rail.Model().Accounts() {
					a.reloadList(acct)
				}
			}
			if !a.v.firstPageShown(gen) {
				return // another chat, or this one again, was opened meanwhile
			}
			logErr("open chat", err) // shown empty then
			a.chat.SetChat(c.acct, c.jid, name, isGroup(c.jid), page)
			for _, m := range a.v.pending {
				a.chat.Upsert(m)
			}
			a.v.pending = nil
			a.markChatRead(c)
		})
	}()
}

// purgeExpired deletes the disappearing messages whose time is up, with their downloads, and
// reports whether any went. Off the UI thread.
func (a *App) purgeExpired() bool {
	paths, n, err := a.db.PurgeExpired(time.Now().Unix())
	logErr("disappearing messages", err)
	for _, p := range paths {
		logErr("disappearing media", a.cache.Remove(p))
	}
	return n > 0
}

// cacheCleared forgets every download after Clear Cache in Settings and shows the open chat again,
// its media downloadable.
func (a *App) cacheCleared() {
	go func() {
		err := a.db.ClearMediaPaths()
		a.mw.Synchronize(func() {
			logErr("forget downloads", err) // the views still treat a missing file as not downloaded
			if c := a.v.open; c.jid != "" {
				a.loadChat(c, a.chatName(c.acct, c.jid))
			}
		})
	}()
}

// loadPrevious answers the chat view's request for the open chat's next older page. A request the
// view makes while it still shows the chat before is not answered: SetChat resets it.
func (a *App) loadPrevious() {
	c, page, gen, ok := a.v.nextPage()
	if !ok {
		return
	}
	go func() {
		msgs, err := a.db.GetChatMessages(c.acct, c.jid, page)
		dropMissingMedia(msgs)
		a.mw.Synchronize(func() {
			if !a.v.current(gen) {
				return // the chat was closed or opened again meanwhile
			}
			logErr("older messages", err) // answered as the end of the history: no retry loop
			if len(msgs) == 0 {
				a.chat.NoMoreHistory(c.acct, c.jid)
			} else {
				a.chat.PrependPage(c.acct, c.jid, msgs)
			}
		})
	}()
}

// markChatRead marks c read: on screen at once, then in the store, then on WhatsApp for its newest
// unread messages (only the user's own devices hear of it when the account's read receipts are off).
func (a *App) markChatRead(c chatRef) {
	a.v.writeStarted(c.acct) // until it is stored, a reloaded chat list would bring the badge back
	if a.side.Model().Account() == c.acct {
		a.side.Model().MarkRead(c.jid)
		a.v.unread[c.acct] = a.side.Model().Unread()
		a.showUnread()
	}
	go func() {
		// shortcut: receipts cover the newest page only; read the unread rows by query if >50 unread matters
		page, err := a.db.GetChatMessages(c.acct, c.jid, 0)
		if err == nil {
			err = a.db.MarkChatRead(c.acct, c.jid)
		}
		a.writeDone(c.acct)
		if err != nil {
			logErr("mark read", err)
			return
		}
		a.sendReceipts(c.acct, unreadIncoming(page))
	}()
}

// markMessageRead marks one message read (a message arriving in the chat being looked at, or a
// status that was viewed), with a read receipt (read-self when the account's are off).
func (a *App) markMessageRead(m store.Message) {
	a.v.writeStarted(m.AccountID)
	go func() {
		err := a.db.UpdateMessageStatus(m.AccountID, m.ChatJID, []string{m.ID}, store.StatusRead)
		a.writeDone(m.AccountID)
		if err != nil {
			logErr("mark read", err)
			return
		}
		a.sendReceipts(m.AccountID, []store.Message{m})
	}()
}

// writeDone tells the UI thread a mark-read write of acct ended (off the UI thread); a chat list
// reload dropped meanwhile runs now.
func (a *App) writeDone(acct string) {
	a.mw.Synchronize(func() {
		if a.v.writeDone(acct) {
			a.reloadList(acct)
		}
	})
}

// sendReceipts tells WhatsApp msgs of acct were read: the senders too while acct's read receipts
// are on; otherwise, or when that setting cannot be read, only the user's own devices (read-self),
// which keeps them in sync without revealing anything. Off the UI thread.
func (a *App) sendReceipts(acct string, msgs []store.Message) {
	if len(msgs) == 0 {
		return
	}
	v, err := ui.Setting(a.db, acct, ui.KeyReadReceipts)
	logErr("read receipts setting (own devices only)", err)
	ctx, cancel := context.WithTimeout(context.Background(), netTimeout)
	defer cancel()
	logErr("read receipts", a.mgr.MarkRead(ctx, msgs, selfOnlyReceipts(v, err)))
}

// needMedia downloads a message's media for the chat view or the status viewer, once at a time per
// message, and hands the file to whichever of them still shows it.
func (a *App) needMedia(m store.Message) {
	k := mediaKey{m.AccountID, m.ChatJID, m.ID}
	if !a.inflight.start(k) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), mediaTimeout)
		path, err := a.mgr.DownloadMedia(ctx, m)
		cancel()
		a.mw.Synchronize(func() {
			a.inflight.done(k)
			if err != nil {
				logErr("download "+m.ID, err)
				return
			}
			if a.v.loaded && a.v.open == (chatRef{m.AccountID, m.ChatJID}) { // IDs repeat across accounts
				a.chat.SetMediaPath(m.ID, path)
			}
			if a.statusSet != nil && a.statusAcct == m.AccountID && m.ChatJID == statusBroadcast {
				a.statusSet(m.ID, path)
			}
		})
	}()
}

// openMedia opens a downloaded file with its Windows program; a file the cache refuses to open is
// reported.
func (a *App) openMedia(path string) {
	if err := a.cache.OpenInDefaultApp(path); err != nil {
		logErr("open media", err)
		a.alert("This file was not opened: " + err.Error())
	}
}

// send sends text to the open chat; the stored message comes back as a MessageEvent. Failed, the
// text goes back in the box if its chat is still open and nothing new was typed.
func (a *App) send(text string) {
	c := a.v.open
	a.sendOff("Message not sent", func(ctx context.Context) error {
		_, err := a.mgr.SendText(ctx, c.acct, c.jid, text)
		return err
	}, netTimeout, func() {
		if a.v.open == c {
			a.bar.Restore(text)
		}
	})
}

// attach sends a picked file to the open chat.
func (a *App) attach(path string) {
	c := a.v.open
	a.sendOff("File not sent", func(ctx context.Context) error {
		_, err := a.mgr.SendMedia(ctx, c.acct, c.jid, path, "")
		return err
	}, mediaTimeout, nil)
}

// sendGIF downloads a picked GIPHY MP4 (allowlist checked again, size capped) and sends it to the
// open chat as a GIF.
func (a *App) sendGIF(mp4URL string) {
	c := a.v.open
	a.sendOff("GIF not sent", func(ctx context.Context) error {
		tmp, err := fetchGIF(ctx, gifHTTP, mp4URL, maxGIFBytes)
		if err != nil {
			return err
		}
		defer func() { logErr("remove GIF", os.Remove(tmp)) }()
		_, err = a.mgr.SendGIF(ctx, c.acct, c.jid, tmp)
		return err
	}, mediaTimeout, nil)
}

// sendOff runs send off the UI thread and reports its failure to the user as what, after failed
// (UI thread; may be nil).
func (a *App) sendOff(what string, send func(ctx context.Context) error, timeout time.Duration, failed func()) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := send(ctx)
		cancel()
		if err != nil {
			log.Printf("app: %s: %v", what, err)
			a.mw.Synchronize(func() {
				if failed != nil {
					failed()
				}
				a.alert(what + ": " + err.Error())
			})
		}
	}()
}

// typing tells the open chat the user is typing (or stopped), unless ghost typing is on or the user
// appears offline.
func (a *App) typing(composing bool) {
	c := a.v.open
	if c.jid == "" || !typingOn(a.cfg[ui.KeyGhostTyping], a.cfg[ui.KeyAppearOffline]) {
		return
	}
	a.queueTyping(func(ctx context.Context) error { return a.mgr.SetTyping(ctx, c.acct, c.jid, composing) })
}

// newChat fetches the active account's contacts off the UI thread, then shows the New Chat picker.
func (a *App) newChat() {
	acct := a.v.active
	if acct == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), netTimeout)
		contacts, err := a.mgr.Contacts(ctx, acct)
		cancel()
		a.mw.Synchronize(func() {
			logErr("contacts", err) // the picker still lists the chats
			if acct != a.v.active {
				return
			}
			logErr("new chat", ui.RunNewChat(a.mw, a.pal, a.side.Model().Chats(), contacts, func(jid string) {
				a.openChat(acct, jid, contactName(contacts, jid))
			}))
		})
	}()
}

func contactName(contacts []whatsapp.Contact, jid string) string {
	for _, c := range contacts {
		if c.JID == jid {
			return c.Name
		}
	}
	return ""
}

// openStory shows sender's status updates of the last 24 hours in the status viewer.
func (a *App) openStory(sender string) {
	acct, name := a.v.active, ""
	for _, s := range a.stories {
		if s.SenderJID == sender {
			name = s.Name
		}
	}
	go func() {
		msgs, err := a.db.StatusMessages(acct, sender, time.Now().Add(-24*time.Hour).Unix())
		dropMissingMedia(msgs)
		a.mw.Synchronize(func() {
			logErr("status updates", err)
			if acct != a.v.active || len(msgs) == 0 || a.statusSet != nil {
				return
			}
			a.statusAcct = acct
			logErr("status viewer", ui.RunStatusView(a.mw, a.pal, sender, name, msgs, ui.StatusCallbacks{
				OnNeedMedia: a.needMedia, OnOpenMedia: a.openMedia, OnViewed: a.markMessageRead,
				Opened: func(set func(id, path string)) { a.statusSet = set },
			}))
			a.statusSet = nil
		})
	}()
}

// senderName names a group member in the chat view.
func (a *App) senderName(jid string) string { return a.chatName(a.v.active, jid) }
