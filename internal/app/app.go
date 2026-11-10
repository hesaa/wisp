// Package app is wa-go's main window: it composes the account rail, sidebar, chat view, input bar,
// tray and dialogs, and wires them to the WhatsApp manager, the message store and the media cache.
//
// Everything here runs on the UI thread except the goroutines it starts for store reads and network
// calls; those hand their results back through the main window's Synchronize, and every manager
// event is marshalled the same way before it touches a widget.
package app

import (
	"context"
	"errors"
	"log"
	"slices"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

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
	SendGIF(ctx context.Context, accountID, chatJID, mp4Path string) (store.Message, error)
	SetTyping(ctx context.Context, accountID, chatJID string, composing bool) error
	SetPresenceAll(ctx context.Context, p types.Presence) error
	MarkRead(ctx context.Context, msgs []store.Message) error
	DownloadMedia(ctx context.Context, msg store.Message) (string, error)
	SetProxy(addr string) error
	Close() error
}

var _ Backend = (*whatsapp.Manager)(nil)

// Timeouts of background calls.
const (
	netTimeout   = 30 * time.Second // receipts, presence, typing, contacts, text
	mediaTimeout = 5 * time.Minute  // uploads and downloads
)

// globalKeys are the app-global settings kept in memory (account "" of the settings table).
var globalKeys = []string{ui.KeyGhostTyping, ui.KeyAppearOffline, ui.KeyAutoDownloadImages, ui.KeyGiphyAPIKey,
	ui.KeyProxy, ui.KeyNotify, ui.KeyNotifyPrivate, ui.KeyNotifySound, ui.KeyTheme}

// App is the running main window.
type App struct {
	db       *store.DB
	cache    *media.Cache
	mgr      Backend
	builtin  string            // the GIPHY key built into the binary
	cfg      map[string]string // globalKeys as last saved
	pal      theme.Palette
	presence presenceState
	signals  chan func() // presence and typing, sent one at a time in order, off the UI thread
	sent     chan struct{}

	mw    *walk.MainWindow
	icon  *walk.Icon
	rail  *ui.Rail
	side  *ui.Sidebar
	empty *ui.EmptyPane
	chat  *ui.ChatView
	bar   *ui.InputBar
	tray  *notify.Tray

	v          view
	stories    []store.Story
	inflight   inflight
	statusSet  func(id, path string) // the open status viewer's SetMediaPath; nil while none is open
	statusAcct string
	pairing    bool
	exiting    bool
}

// New builds the main window, themed by the settings in db; nothing connects until Attach.
// builtinGIFKey is the GIPHY key used when Settings has none.
func New(db *store.DB, cache *media.Cache, builtinGIFKey string) (*App, error) {
	a := &App{db: db, cache: cache, builtin: builtinGIFKey, cfg: map[string]string{},
		v: view{unread: map[string]int{}}, inflight: inflight{}}
	for _, k := range globalKeys {
		v, err := ui.Setting(db, "", k)
		logErr("read setting "+k, err) // the default stands in
		a.cfg[k] = v
	}
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
	if err := errors.Join(l.SetMargins(walk.Margins{}), l.SetSpacing(0), mw.SetLayout(l), mw.SetTitle("wa-go"), mw.SetIcon(a.icon),
		mw.SetMinMaxSize(walk.Size{Width: 820, Height: 560}, walk.Size{}), mw.SetSize(walk.Size{Width: 1180, Height: 760})); err != nil {
		return err
	}
	if a.rail, err = ui.NewRail(mw, p); err != nil {
		return err
	}
	if a.side, err = ui.NewSidebar(mw, p); err != nil {
		return err
	}
	right, err := walk.NewComposite(mw) // the chat column: the chat over the input bar, or the empty state
	if err != nil {
		return err
	}
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
	if err := errors.Join(rl.SetStretchFactor(a.empty, 1000), rl.SetStretchFactor(a.chat, 1000)); err != nil {
		return err
	}
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
	a.bar.OnSend(a.send)
	a.bar.OnTyping(a.typing)
	a.bar.OnAttach(a.attach)
	a.bar.OnSendGIF(a.sendGIF)
	mw.Closing().Attach(a.closing)
	mw.Activating().Attach(a.activated)
	// the focus can only be set once the window runs; the search box takes it, without a focus ring
	mw.Starting().Attach(func() { mw.Synchronize(func() { logErr("focus", a.side.FocusSearch()) }) })
	mw.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	// cosmetic: Windows builds without dark title bars keep a light one
	logErr("title bar", theme.ApplyTitleBar(uintptr(mw.Handle()), theme.Resolve(mode) == theme.ThemeDark))
	return nil
}

// Attach connects mgr: the saved proxy first, then the event listener, then every saved account
// (in the background). With no account at all the pairing dialog opens.
func (a *App) Attach(mgr Backend) {
	a.mgr = mgr
	logErr("proxy", mgr.SetProxy(a.cfg[ui.KeyProxy])) // ValidateProxy errors never repeat the address
	mgr.AddListener(a.listen)
	a.signals, a.sent = make(chan func(), 64), make(chan struct{})
	go func() {
		for f := range a.signals {
			f()
		}
		close(a.sent)
	}()
	go func() {
		err := mgr.Start(context.Background())
		a.mw.Synchronize(func() {
			logErr("connect", err) // accounts that failed keep retrying; the rail shows them offline
			a.refreshAccounts()
			if len(a.rail.Model().Accounts()) == 0 {
				a.pair()
			}
		})
	}()
}

// Run shows the window and the tray icon and returns after Exit, once every account has been set
// unavailable and disconnected. Call Attach first.
func (a *App) Run() error {
	tray, err := notify.NewTray(a.mw, a.icon, "wa-go", a.restore, a.exit)
	if err != nil {
		return err
	}
	a.tray = tray
	a.tray.OnToastClick(a.toastClicked)
	a.mw.Show()
	a.mw.Run()
	// the window is gone: blocking here is fine
	if a.tray != nil { // the loop ended some other way than Exit
		logErr("tray", a.tray.Dispose())
	}
	close(a.signals)
	<-a.sent
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	logErr("presence", a.mgr.SetPresenceAll(ctx, types.PresenceUnavailable))
	cancel()
	logErr("disconnect", a.mgr.Close())
	a.icon.Dispose()
	return nil
}

// closing hides the window to the tray instead of closing it, and every account goes unavailable at
// once; only Exit closes it.
func (a *App) closing(canceled *bool, _ walk.CloseReason) {
	if a.exiting {
		return
	}
	*canceled = true
	a.mw.Hide()
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

// restore brings the window back from the tray (or a minimised state) to the front.
func (a *App) restore() {
	if !a.mw.Visible() {
		a.mw.Show()
	}
	h := a.mw.Handle()
	if win.IsIconic(h) {
		win.ShowWindow(h, win.SW_RESTORE)
	}
	win.SetForegroundWindow(h)
}

// activated: the user is back; show them available (unless appearing offline) and read the open chat.
func (a *App) activated() {
	a.sendPresence(false)
	if c := a.v.open; c.jid != "" && a.v.loaded && a.chatUnread(c.jid) > 0 {
		a.markChatRead(c)
	}
}

// foreground reports whether the user is looking at the window.
func (a *App) foreground() bool {
	h := a.mw.Handle()
	return a.mw.Visible() && !win.IsIconic(h) && win.GetForegroundWindow() == h
}

// sendPresence broadcasts the presence the window's state calls for (see presenceState.next).
func (a *App) sendPresence(force bool) {
	p, send := a.presence.next(a.mw.Visible(), a.cfg[ui.KeyAppearOffline] == "true", force)
	if send && slices.ContainsFunc(a.rail.Model().Accounts(), a.rail.Model().Connected) { // a reconnect says it again
		a.signal("presence", func(ctx context.Context) error { return a.mgr.SetPresenceAll(ctx, p) })
	}
}

// signal queues f behind the presence and typing updates already queued.
func (a *App) signal(what string, f func(ctx context.Context) error) {
	select {
	case a.signals <- func() {
		ctx, cancel := context.WithTimeout(context.Background(), netTimeout)
		defer cancel()
		logErr(what, f(ctx))
	}:
	default:
		log.Printf("app: %s dropped: too many updates waiting", what)
	}
}

// nav handles the rail's Chats, Status and Settings buttons.
func (a *App) nav(n ui.Nav) {
	switch n {
	case ui.NavChats:
		logErr("focus", a.side.FocusSearch())
	case ui.NavStatus:
		if len(a.stories) > 0 {
			a.openStory(a.stories[0].SenderJID)
		}
	case ui.NavSettings:
		logErr("settings", ui.RunSettings(a.mw, a.pal, ui.SettingsDeps{Store: a.db, Manager: a.mgr, Media: a.cache, Account: a.v.active,
			OnThemeChanged: a.applyTheme, OnSettingsChanged: a.settingChanged, OnAddAccount: a.pair}))
	}
}

// applyTheme recolours every widget and the title bar.
func (a *App) applyTheme(mode theme.ThemeMode) {
	a.pal = theme.GetPalette(mode)
	logErr("theme", errors.Join(a.rail.SetPalette(a.pal), a.side.SetPalette(a.pal), a.chat.SetPalette(a.pal),
		a.bar.SetPalette(a.pal), a.empty.SetPalette(a.pal)))
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
	case ui.KeyAppearOffline:
		a.sendPresence(true)
	case ui.KeyAutoDownloadImages:
		a.chat.SetAutoDownload(v == "true")
	}
}

// pair runs the QR pairing dialog; the paired account arrives as a ConnectionEvent.
func (a *App) pair() {
	if a.pairing {
		return
	}
	a.pairing = true
	defer func() { a.pairing = false }()
	if _, err := ui.RunQRDialog(a.mw, a.pal, a.mgr.Pair); err != nil {
		logErr("pairing", err)
		a.alert("The pairing window could not open: " + err.Error())
	}
}

// alert shows a problem the user should know about over the window in front.
func (a *App) alert(text string) {
	var owner walk.Form = a.mw
	if f := walk.App().ActiveForm(); f != nil {
		owner = f
	}
	walk.MsgBox(owner, "wa-go", text, walk.MsgBoxIconWarning|walk.MsgBoxOK)
}

// logErr logs a failure that has no caller to return it to.
func logErr(op string, err error) {
	if err != nil {
		log.Printf("app: %s: %v", op, err)
	}
}
