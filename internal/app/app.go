// Package app is Wisp's main window: it composes the account rail, sidebar, chat view, input bar,
// tray and dialogs, and wires them to the WhatsApp manager, the message store and the media cache.
//
// Everything here runs on the UI thread except the goroutines it starts for store reads and network
// calls; those hand their results back through the main window's Synchronize, and every manager
// event is marshalled the same way before it touches a widget.
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"wa-go/internal/lock"
	"wa-go/internal/media"
	"wa-go/internal/notify"
	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui"
	"wa-go/internal/whatsapp"
)

// Backend is the part of *whatsapp.Manager the app uses.
type Backend interface {
	ui.SettingsManager
	AddListener(fn func(evt any))
	Start(ctx context.Context) error
	Pair(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	Contacts(ctx context.Context, accountID string) ([]whatsapp.Contact, error)
	SendText(ctx context.Context, accountID, chatJID, text string) (store.Message, error)
	SendMedia(ctx context.Context, accountID, chatJID, path, caption string) (store.Message, error)
	SendGIF(ctx context.Context, accountID, chatJID, mp4Path string, preview []byte, w, h int) (store.Message, error)
	SetTyping(ctx context.Context, accountID, chatJID string, composing bool) error
	SetPresenceAll(ctx context.Context, p types.Presence) error
	MarkRead(ctx context.Context, msgs []store.Message, selfOnly bool) error
	DownloadMedia(ctx context.Context, msg store.Message) (string, error)
	ProfilePicture(ctx context.Context, accountID, jid, knownID string, full bool) (whatsapp.Picture, bool, error)
	FetchPicture(ctx context.Context, rawURL string) ([]byte, error)
	About(ctx context.Context, accountID, jid string) (string, error)
	GroupDetails(ctx context.Context, accountID, jid string) (whatsapp.GroupDetails, error)
	SetProxy(addr string) error
	Close() error
}

var _ Backend = (*whatsapp.Manager)(nil)

// Timeouts of background calls.
const (
	netTimeout   = 30 * time.Second // receipts, presence, typing, contacts, text
	mediaTimeout = 5 * time.Minute  // uploads and downloads
	exitTimeout  = 3 * time.Second  // the last unavailable broadcast at Exit
)

// globalKeys are the app-global settings kept in memory (account "" of the settings table).
var globalKeys = []string{ui.KeyGhostTyping, ui.KeyAppearOffline, ui.KeyAutoDownloadImages, ui.KeyGiphyAPIKey,
	ui.KeyProxy, ui.KeyNotify, ui.KeyNotifyPrivate, ui.KeyNotifySound, ui.KeyNotifySoundName, ui.KeyTheme, ui.KeyLockPIN}

// App is the running main window.
type App struct {
	db       *store.DB
	cache    *media.Cache
	mgr      Backend
	builtin  string            // the GIPHY key built into the binary
	cfg      map[string]string // globalKeys as last saved
	pal      theme.Palette
	presence presenceCtl
	outbox   *presenceBox                     // the presence to broadcast next, never dropped
	typingQ  chan func(context.Context) error // typing updates, sent one at a time in order
	bg       context.Context                  // ends at Exit: queued typing is skipped, calls under way stop
	stopBG   context.CancelFunc

	mw    *walk.MainWindow
	icon  *walk.Icon
	rail  *ui.Rail
	side  *ui.Sidebar
	empty *ui.EmptyPane
	chat  *ui.ChatView
	bar   *ui.InputBar
	media *ui.MediaView
	info  *ui.InfoView
	tray  *notify.Tray

	avatars *ui.Avatars
	avatarQ *avatarQueue

	v          view
	stories    []store.Story
	inflight   inflight
	statusSet  func(id, path string) // the open status viewer's SetMediaPath; nil while none is open
	statusAcct string
	right      *walk.Composite // the chat column
	link       *ui.LinkPage
	lockPage   *ui.LockPage
	backoff    lock.Backoff // wrong PINs; not persisted
	adding     bool         // the user asked to link another account
	loaded     bool         // the saved accounts are on the rail: until then none is not "no account"
	page       page         // as showPage last set it
	locked     bool         // the lock page covers the window (Task 5)
	exiting    bool
	resetting  bool // Reset Wisp's confirmation, logout and exit are under way: the lock leaves its dialog be
	didReset   bool // the run ends through Reset Wisp
	hiding     bool // closed to the tray and not restored since
	minimized   bool // as sizeChanged last saw the window
	viewing     bool // the media view replaces the chat and the input bar
	showingInfo bool // the details view replaces the chat and the input bar
	infoGen     int
}

// New builds the main window, themed by the settings in db; nothing connects until Attach.
// builtinGIFKey is the GIPHY key used when Settings has none.
func New(db *store.DB, cache *media.Cache, builtinGIFKey string) (*App, error) {
	a := &App{db: db, cache: cache, builtin: builtinGIFKey, cfg: map[string]string{},
		v: view{unread: map[string]int{}, names: map[string]map[string]string{}, dirs: map[string]*ui.Directory{}}, inflight: inflight{}}
	a.presence = presenceCtl{
		window:        func() (bool, bool) { return a.mw.Visible(), win.IsIconic(a.mw.Handle()) },
		appearOffline: func() bool { return a.cfg[ui.KeyAppearOffline] == "true" },
		send: func(p types.Presence) {
			if a.outbox != nil { // nil until Attach
				a.outbox.put(p)
			}
		},
		grace: grace{after: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, func() { a.mw.Synchronize(f) }).Stop
		}},
	}
	for _, k := range globalKeys {
		v, err := ui.Setting(db, "", k)
		if err != nil && k == ui.KeyLockPIN {
			return nil, fmt.Errorf("app: reading the app lock: %w", err) // fail safe: never start unlocked by mistake
		}
		logErr("read setting "+k, err) // the default stands in
		a.cfg[k] = v
	}
	a.avatars = ui.NewAvatars(cache)
	ui.SetAvatars(a.avatars)
	a.avatarQ = newAvatarQueue()
	a.avatarQ.wanted = func(account, jid string) bool {
		return a.fetchAllowed()
	}
	a.avatarQ.fetch = func(account, jid string) pictureResult {
		var knownID string
		if d := a.dir(account); d != nil {
			if p, ok := d.Person(jid); ok {
				knownID = p.AvatarID
			}
		}
		bg := a.bg
		if bg == nil {
			bg = context.Background()
		}
		ctx, cancel := context.WithTimeout(bg, 20*time.Second)
		defer cancel()
		return fetchAvatarResult(ctx, a.mgr, account, jid, knownID)
	}
	a.avatarQ.onDrop = func(account, jid string) {
		if a.avatars != nil {
			a.avatars.Invalidate(jid)
		}
	}
	a.avatarQ.onDone = func(account, jid string, res pictureResult) {
		var p store.Person
		if d := a.dir(account); d != nil {
			p, _ = d.Person(jid)
		}
		id, ts, save := avatarStep(p, time.Now(), res)
		if save && len(res.data) > 0 {
			if _, err := a.cache.SaveAvatar(account, jid, res.data); err != nil {
				logErr("save avatar "+jid, err)
			}
		}
		if err := a.db.SetAvatar(account, jid, id, ts); err != nil {
			logErr("db set avatar "+jid, err)
		}
		if a.avatars != nil {
			a.avatars.Invalidate(jid)
		}
		if a.mw != nil {
			a.mw.Synchronize(func() {
				if a.side != nil {
					a.side.Invalidate()
				}
				if a.chat != nil {
					a.chat.Invalidate()
				}
				if a.info != nil {
					a.info.Invalidate()
				}
				if a.rail != nil {
					a.rail.Invalidate()
				}
			})
		}
	}
	a.avatars.OnWant(func(jid string) {
		if !a.fetchAllowed() {
			a.avatars.Invalidate(jid)
			return
		}
		acct := a.v.active
		if acct == "" {
			a.avatars.Invalidate(jid)
			return
		}
		if d := a.dir(acct); d != nil {
			if p, ok := d.Person(jid); ok {
				if !ui.AvatarDue(p, time.Now()) {
					return
				}
			}
		}
		a.avatarQ.want(acct, jid)
	})
	if err := a.build(); err != nil {
		if a.mw != nil {
			a.mw.Dispose()
		}
		return nil, err
	}
	return a, nil
}

func (a *App) build() error {
	mode := ui.ParseTheme(a.cfg[ui.KeyTheme])
	a.pal = theme.GetPalette(mode)
	p := a.pal
	mw, err := walk.NewMainWindow()
	if err != nil {
		return err
	}
	a.mw = mw
	if a.icon, err = walk.NewIconFromResource("APP"); err != nil {
		logErr("app icon", err)
		a.icon = walk.IconApplication()
	}
	l := walk.NewHBoxLayout()
	if err := errors.Join(l.SetMargins(walk.Margins{}), l.SetSpacing(0), mw.SetLayout(l), mw.SetTitle("Wisp"), mw.SetIcon(a.icon),
		mw.SetMinMaxSize(walk.Size{Width: 820, Height: 560}, walk.Size{}), mw.SetSize(walk.Size{Width: 1180, Height: 760})); err != nil {
		return err
	}
	if a.rail, err = ui.NewRail(mw, p); err != nil {
		return err
	}
	if a.side, err = ui.NewSidebar(mw, p); err != nil {
		return err
	}
	right, err := walk.NewComposite(mw) // the chat column: the chat over the input bar, the media view, or the empty state
	if err != nil {
		return err
	}
	a.right = right
	rl := walk.NewVBoxLayout()
	if err := errors.Join(rl.SetMargins(walk.Margins{}), rl.SetSpacing(0), right.SetLayout(rl), l.SetStretchFactor(right, 1000)); err != nil {
		return err
	}
	if a.empty, err = ui.NewEmptyPane(right, p, "Choose a chat, or start a new one."); err != nil {
		return err
	}
	if a.chat, err = ui.NewChatView(right, p, a.cache, a.senderName); err != nil {
		return err
	}
	if a.bar, err = ui.NewInputBar(right, p, func() string { return gifKey(a.cfg[ui.KeyGiphyAPIKey], a.builtin) }); err != nil {
		return err
	}
	if a.media, err = ui.NewMediaView(right, p); err != nil {
		return err
	}
	if a.info, err = ui.NewInfoView(right, p); err != nil {
		return err
	}
	if err := errors.Join(rl.SetStretchFactor(a.empty, 1000), rl.SetStretchFactor(a.chat, 1000), rl.SetStretchFactor(a.media, 1000), rl.SetStretchFactor(a.info, 1000)); err != nil {
		return err
	}
	if a.link, err = ui.NewLinkPage(mw, p, func(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) { return a.mgr.Pair(ctx) }); err != nil {
		return err
	}
	if err := l.SetStretchFactor(a.link, 1000); err != nil {
		return err
	}
	if a.lockPage, err = ui.NewLockPage(mw, p); err != nil {
		return err
	}
	if err := l.SetStretchFactor(a.lockPage, 1000); err != nil {
		return err
	}
	a.lockPage.OnUnlock(a.unlock)
	a.lockPage.OnReset(a.reset)
	a.link.OnCancel(func() { a.adding = false; a.showPage() })
	a.showChatPane(false)
	a.chat.SetAutoDownload(a.cfg[ui.KeyAutoDownloadImages] == "true")

	a.rail.Model().OnActiveChanged(a.accountChanged)
	a.rail.OnAddAccount(a.pair)
	a.rail.OnNav(a.nav)
	a.side.OnChatSelected(func(jid string) { a.openChat(a.v.active, jid, "") })
	a.side.OnStorySelected(a.openStory)
	a.side.OnNewChat(a.newChat)
	a.chat.OnLoadPrevious(a.loadPrevious)
	a.chat.OnNeedMedia(a.needMedia)
	a.chat.OnOpenMedia(a.openMedia)
	a.media.OnClose(a.closeMedia)
	a.media.OnNeedMedia(a.needMedia)
	a.media.OnOpenWith(a.openFile)
	a.info.OnClose(a.closeInfo)
	a.info.OnRetry(a.loadInfo)
	a.info.OnOpenChat(func(jid string) {
		a.openChat(a.v.active, jid, "")
	})
	a.chat.OnOpenInfo(a.openInfo)
	a.bar.OnSend(a.send)
	a.bar.OnTyping(a.typing)
	a.bar.OnAttach(a.attach)
	a.bar.OnSendGIF(a.sendGIF)
	mw.Closing().Attach(a.closing)
	mw.Activating().Attach(a.activated)
	mw.SizeChanged().Attach(a.sizeChanged)
	// the focus can only be set once the window runs; the search box takes it, without a focus ring
	mw.Starting().Attach(func() { mw.Synchronize(a.focusPage) })
	mw.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	logErr("presence", a.watchActivation()) // without it presence stays unavailable
	if a.cfg[ui.KeyLockPIN] != "" {
		logErr("taskbar preview", forceIconic(mw.Handle(), true)) // not fatal: the lock page still covers a restore
	}
	// cosmetic: Windows builds without dark title bars keep a light one
	logErr("title bar", theme.ApplyTitleBar(uintptr(mw.Handle()), theme.Resolve(mode) == theme.ThemeDark))
	return nil
}

// Attach connects mgr: the saved proxy first, then the event listener, then every saved account
// (in the background). With no account at all the link page shows.
func (a *App) Attach(mgr Backend) {
	a.mgr = mgr
	logErr("proxy", mgr.SetProxy(a.cfg[ui.KeyProxy])) // ValidateProxy errors never repeat the address
	ui.SetGIFProxy(a.cfg[ui.KeyProxy])
	mgr.AddListener(a.listen)
	a.bg, a.stopBG = context.WithCancel(context.Background())
	a.outbox, a.typingQ = newPresenceBox(), make(chan func(context.Context) error, 64)
	go a.presenceWorker()
	go a.typingWorker()
	a.lockNow() // before the window is first shown: the chats never appear
	go func() {
		_ = a.db.ResetEmptyAvatars()
		a.purgeExpired()                       // before any chat list is read
		err := mgr.Start(context.Background()) // returns before the accounts connect
		a.mw.Synchronize(func() {
			logErr("sessions", err)
			a.loaded = true
			a.refreshAccounts()
		})
	}()
}

// Run shows the window and the tray icon and returns after Exit, once every account has been set
// unavailable and disconnected. Call Attach first.
func (a *App) Run() (reset bool, err error) {
	a.mw.Show()
	a.showPage() // the first page may have been picked before the window was visible
	a.mw.Synchronize(func() {
		tray, terr := notify.NewTray(a.mw, a.icon, "Wisp", a.restore, a.exit)
		if terr != nil {
			logErr("tray", terr)
		} else {
			a.tray = tray
			a.tray.OnToastClick(a.toastClicked)
		}
	})
	a.mw.Run()
	// the window is gone: blocking here is fine
	if a.tray != nil { // the loop ended some other way than Exit
		logErr("tray", a.tray.Dispose())
	}
	a.stopBG() // queued typing is skipped; a broadcast under way stops, so it cannot land after this one
	ctx, cancel := context.WithTimeout(context.Background(), exitTimeout)
	logErr("presence", a.mgr.SetPresenceAll(ctx, types.PresenceUnavailable))
	cancel()
	logErr("disconnect", a.mgr.Close())
	a.icon.Dispose()
	return a.didReset, nil
}

// closing hides the window to the tray instead of closing it, and every account goes unavailable at
// once; only Exit closes it.
func (a *App) closing(canceled *bool, _ walk.CloseReason) {
	if a.exiting || a.tray == nil {
		return
	}
	*canceled = true
	a.mw.Hide()
	a.hiding = true
	a.lockNow() // locked while hidden: a restore never flashes the chats
	// closing an owned dialog shows its owner again: hide once they are gone
	a.mw.Synchronize(func() {
		if a.hiding && a.mw.Visible() {
			a.mw.Hide()
			a.showPage()
		}
	})
	a.showPage() // stops pairing while hidden
	a.sendPresence(true)
}

// exit quits from the tray's Exit item.
func (a *App) exit() {
	a.mw.Synchronize(func() { // not from inside the tray's own menu handling
		if a.exiting {
			return // Exit picked twice before the window closed
		}
		a.exiting = true
		logErr("tray", a.tray.Dispose())
		a.tray = nil
		logErr("close", a.mw.Close())
	})
}

// Restore brings the window to the front from any goroutine (another Wisp was started).
func (a *App) Restore() { a.mw.Synchronize(a.restore) }

// restore brings the window back from the tray (or a minimised state) to the front.
func (a *App) restore() {
	a.hiding = false
	if !a.mw.Visible() {
		a.mw.Show()
	}
	h := a.mw.Handle()
	if win.IsIconic(h) {
		win.ShowWindow(h, win.SW_RESTORE)
	}
	win.SetForegroundWindow(h)
	a.showPage() // pairing resumes
	if a.page == pageLock {
		a.lockPage.Focus()
	}
}

// activated: the user is back on the main window; read the open chat. (Presence follows
// WM_ACTIVATEAPP: see watchActivation.)
func (a *App) activated() {
	if a.locked {
		if !a.resetting {
			closeModals(a.mw.Handle()) // none survives a lock, whatever brought it back
		}
		a.focusPage() // walk put the focus back into the hidden chats
		return
	}
	if a.page != pageChats {
		return // the chat is hidden behind the link page
	}
	if c := a.v.open; c.jid != "" && a.v.loaded && a.chatUnread(c.jid) > 0 {
		a.markChatRead(c)
	}
}

// foreground reports whether the user is looking at the window.
func (a *App) foreground() bool {
	h := a.mw.Handle()
	return inFront(a.mw.Visible(), win.IsIconic(h), a.locked || a.page != pageChats, win.GetForegroundWindow() == h)
}

// sendPresence broadcasts the presence the window's state calls for (see presenceCtl.update).
// Accounts not connected yet fail (logged); a reconnect broadcasts again.
func (a *App) sendPresence(force bool) { a.presence.update(force) }

// sizeChanged follows minimising and restoring (walk publishes SizeChanged from WM_WINDOWPOSCHANGED,
// which every route sends: the minimise button, the taskbar button, Win+Down): minimised, every
// account goes unavailable at once, as when closed to the tray; restored, available again unless
// appearing offline.
func (a *App) fetchAllowed() bool {
	visible := a.mw != nil && a.mw.Visible() && !a.hiding
	return fetchAllowed(visible, a.minimized, a.locked)
}

func (a *App) dir(acct string) *ui.Directory {
	if a.v.dirs != nil {
		return a.v.dirs[acct]
	}
	return nil
}

func (a *App) sizeChanged() {
	if m := win.IsIconic(a.mw.Handle()); m != a.minimized {
		a.minimized = m
		a.sendPresence(m)
		if m {
			a.lockNow() // locked while minimised: a restore never shows the chats
		}
		a.showPage() // pairing stops while minimised
	}
}

// presenceWorker broadcasts the latest presence put in the outbox, one broadcast at a time.
func (a *App) presenceWorker() {
	for {
		select {
		case <-a.bg.Done():
			return
		case <-a.outbox.wake:
			if p, ok := a.outbox.take(); ok && a.bg.Err() == nil {
				ctx, cancel := context.WithTimeout(a.bg, netTimeout)
				logErr("presence", a.mgr.SetPresenceAll(ctx, p))
				cancel()
			}
		}
	}
}

// queueTyping queues a typing update behind the ones waiting; one lost when 64 wait does no harm
// (WhatsApp ends a typing indicator by itself).
func (a *App) queueTyping(f func(ctx context.Context) error) {
	select {
	case a.typingQ <- f:
	default:
		log.Printf("app: typing update dropped: too many waiting")
	}
}

func (a *App) typingWorker() {
	for {
		select {
		case <-a.bg.Done():
			return
		case f := <-a.typingQ:
			if a.bg.Err() != nil {
				return // Exit: what is still queued is skipped
			}
			ctx, cancel := context.WithTimeout(a.bg, netTimeout)
			logErr("typing", f(ctx))
			cancel()
		}
	}
}

// nav handles the rail's Chats, Status and Settings buttons.
func (a *App) nav(n ui.Nav) {
	if a.locked {
		return // nothing is shown, opened or sent behind the lock page
	}
	switch n {
	case ui.NavChats:
		a.side.FocusSearch()
	case ui.NavStatus:
		if len(a.stories) > 0 {
			a.openStory(a.stories[0].SenderJID)
		}
	case ui.NavSettings:
		logErr("settings", ui.RunSettings(a.mw, a.pal, ui.SettingsDeps{Store: a.db, Manager: a.mgr, Media: a.cache, Account: a.v.active,
			OnThemeChanged: a.applyTheme, OnSettingsChanged: a.settingChanged, OnAddAccount: a.pair, OnCacheCleared: a.cacheCleared}))
	}
}

// applyTheme recolours every widget and the title bar.
func (a *App) applyTheme(mode theme.ThemeMode) {
	a.pal = theme.GetPalette(mode)
	logErr("theme", errors.Join(a.rail.SetPalette(a.pal), a.side.SetPalette(a.pal), a.chat.SetPalette(a.pal),
		a.bar.SetPalette(a.pal), a.empty.SetPalette(a.pal), a.link.SetPalette(a.pal), a.media.SetPalette(a.pal), a.info.SetPalette(a.pal), a.lockPage.SetPalette(a.pal)))
	logErr("title bar", theme.ApplyTitleBar(uintptr(a.mw.Handle()), theme.Resolve(mode) == theme.ThemeDark))
}

// settingChanged re-reads an app-global setting saved in Settings and applies it. Values are never
// logged: they include the GIPHY key and proxy passwords.
func (a *App) settingChanged(key string) {
	v, err := ui.Setting(a.db, "", key)
	if err != nil {
		logErr("read setting "+key, err)
		return
	}
	a.cfg[key] = v
	switch key {
	case ui.KeyProxy:
		logErr("proxy", a.mgr.SetProxy(v))
		ui.SetGIFProxy(v)
	case ui.KeyAppearOffline:
		a.sendPresence(true)
	case ui.KeyAutoDownloadImages:
		a.chat.SetAutoDownload(v == "true")
	case ui.KeyLockPIN:
		logErr("taskbar preview", forceIconic(a.mw.Handle(), v != ""))
	}
}

// pair shows the link page over the chats; the paired account arrives as a ConnectionEvent.
func (a *App) pair() {
	if a.locked {
		return // nothing is shown, opened or sent behind the lock page
	}
	a.adding = true
	a.showPage()
}

// showPage shows at most one page: the chats, the link page or the lock page (none until the
// accounts are loaded). Pairing runs only while the link page is in front of a window the user can
// see; a video or GIF plays only while the chats are.
func (a *App) showPage() {
	n := len(a.rail.Model().Accounts())
	pg := pageFor(n, a.loaded, a.locked, a.adding)
	if !mediaStays(pg, a.mw.Visible(), win.IsIconic(a.mw.Handle())) {
		a.closeMedia() // to the tray, minimised, linking: nothing plays unseen
		a.closeInfo()
	}
	chats := pg == pageChats
	a.rail.SetVisible(chats)
	a.side.SetVisible(chats)
	a.right.SetVisible(chats)
	if pairingRuns(pg, a.mw.Visible(), win.IsIconic(a.mw.Handle())) {
		a.link.Show(linkCancellable(a.adding, n))
	} else {
		a.link.Hide()
	}
	a.lockPage.SetVisible(pg == pageLock)
	if pg != a.page {
		a.page = pg
		a.focusPage() // the focused Cancel / Try again / PIN box is gone
	}
}

// focusPage puts the keyboard where the page in front wants it, once the window is on screen.
func (a *App) focusPage() {
	if !a.mw.Visible() || win.IsIconic(a.mw.Handle()) || !win.IsWindowEnabled(a.mw.Handle()) {
		return // an open dialog keeps the window disabled; activating it again retries
	}
	switch a.page {
	case pageChats:
		a.side.FocusSearch()
	case pageLock:
		a.lockPage.Focus()
	}
}

// lockNow covers the window with the lock page; it does nothing while no PIN is set.
func (a *App) lockNow() {
	if a.cfg[ui.KeyLockPIN] == "" || a.locked {
		return // off, or already locked (a countdown in progress stays)
	}
	a.locked = true
	endMenu.Call() // an open context menu could still Copy behind the lock
	if !a.resetting {
		closeModals(a.mw.Handle()) // Settings, New chat, the status viewer, their dialogs
	}
	a.mw.Synchronize(a.focusPage) // once they are gone
	a.closeMedia()
	a.closeInfo()
	a.lockPage.Clear()
	a.showPage()
	a.presence.appLock(true)
}

// unlock checks pin against the stored hash, with the back-off for wrong tries. pin is not kept.
func (a *App) unlock(pin string) {
	if d := a.backoff.Wait(time.Now()); d > 0 {
		a.lockPage.Wait(d) // a refused attempt does not count
		return
	}
	ok, err := lock.Verify(a.cfg[ui.KeyLockPIN], pin)
	logErr("PIN check", err) // a malformed stored value; the text holds no PIN
	if ok {
		a.backoff.Succeeded()
		a.locked = false
		a.lockPage.Clear()
		a.showPage() // focuses the search box
		a.presence.appLock(false)
		a.activated() // the chat in front is read now
		return
	}
	now := time.Now()
	a.backoff.Failed(now)
	a.lockPage.Clear()
	a.lockPage.Say("That PIN isn't right.")
	if d := a.backoff.Wait(now); d > 0 {
		a.lockPage.Wait(d)
	}
}

// reset is the lock page's Reset Wisp: after a confirmation it logs every account out and exits;
// main then deletes the data and starts a new Wisp. The confirmation is the one dialog the lock
// leaves open.
func (a *App) reset() {
	if a.resetting {
		return
	}
	a.resetting = true
	ok, err := ui.Confirm(a.mw, a.pal, "Reset Wisp?",
		"This logs out every account on this PC and deletes your chats, media and settings here. Your phone keeps its chats.", "Reset")
	if err != nil || !ok {
		logErr("reset confirm", err)
		a.resetting = false
		return
	}
	failed := logoutAll(a.bg, a.rail.Model().Accounts(), func(ctx context.Context, id string) error {
		ctx, cancel := context.WithTimeout(ctx, netTimeout)
		defer cancel()
		return a.mgr.Logout(ctx, id)
	})
	if len(failed) > 0 {
		a.alert(`Some accounts couldn't be logged out. Remove "Wisp" under Linked devices on your phone.`)
	}
	a.didReset = true
	a.exit()
}

// alert shows a problem the user should know about over the window in front.
func (a *App) alert(text string) {
	var owner walk.Form = a.mw
	if f := walk.App().ActiveForm(); f != nil {
		owner = f
	}
	walk.MsgBox(owner, "Wisp", text, walk.MsgBoxIconWarning|walk.MsgBoxOK)
}

// logErr logs a failure that has no caller to return it to.
func logErr(op string, err error) {
	if err != nil {
		log.Printf("app: %s: %v", op, err)
	}
}
