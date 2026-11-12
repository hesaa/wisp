package ui

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"wa-go/internal/media"
	"wa-go/internal/notify"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
	"wa-go/internal/whatsapp"
)

// SettingsManager is the part of *whatsapp.Manager the Settings dialog uses.
type SettingsManager interface {
	PrivacyClient
	Accounts() []string
	Logout(ctx context.Context, accountID string) error
}

// SettingsDeps is what the Settings dialog works with. Store, Manager and Media are required.
// The callbacks run on the UI thread, possibly after the dialog has closed (a save still under way
// then completes).
type SettingsDeps struct {
	Store   SettingsStore
	Manager SettingsManager
	Media   *media.Cache
	Account string // the active account: the Privacy section starts on it

	OnThemeChanged    func(theme.ThemeMode) // a theme was picked: apply it now (it is saved after)
	OnSettingsChanged func(key string)      // an app-global setting (a Key* constant) was saved: re-read and apply it
	OnAddAccount      func()                // Add account was chosen; the dialog has closed
	OnCacheCleared    func()                // the media cache was cleared (fully or not): forget the downloads
}

// Sizes in 1/96 inch.
const (
	settingsW = 780
	settingsH = 580
	navW      = 220
	navRowH   = 44
	toggleH   = 56
	choiceH   = 64
	fieldH    = 36 // text fields and buttons
)

var sectionNames = [...]string{"Privacy & Security", "Chats & Presence", "Media & Storage", "Network & Proxy", "Accounts & Sessions", "Notifications & Theme"}

const (
	secPrivacy = iota
	secChats
	secMedia
	secNetwork
	secAccounts
	secNotify
)

// RunSettings shows the modal Settings dialog over owner: a section list on the left (Up/Down
// switch sections; the focus starts there) and the section's controls on the right, every one a
// tab stop. Privacy settings belong to one account (a picker shows with two or more) and are saved
// on WhatsApp before this PC; the rest are app-global. Esc or closing ends it. UI-thread only.
func RunSettings(owner walk.Form, p theme.Palette, deps SettingsDeps) error {
	dlg, err := walk.NewDialogWithFixedSize(owner)
	if err != nil {
		return err
	}
	d := &settingsDialog{Dialog: dlg, kit: &kit{pal: p}, owner: owner, deps: deps,
		vals: map[string]string{}, privacy: map[string]valueWidget{}, globals: map[string]valueWidget{}}
	d.ctl = &settingsCtl{store: deps.Store, wa: deps.Manager, bg: onWorker(&settingsState.jobs, owner.Synchronize),
		onTheme: d.themeChanged, shared: &settingsState, closed: func() bool { return d.closed },
		onChanged: func(key string) {
			if deps.OnSettingsChanged != nil {
				deps.OnSettingsChanged(key)
			}
		}}
	if err := d.build(); err != nil {
		dlg.Dispose()
		return err
	}
	dlg.Run()
	d.closed = true
	dlg.Dispose()
	if d.addAccount && deps.OnAddAccount != nil {
		deps.OnAddAccount()
	}
	return nil
}

// settingsState is shared by every Settings dialog: a dialog reopened while a change made in the
// last one is still being saved queues behind it and shows its outcome.
var settingsState settingsShared

// settingsDialog is the open Settings dialog. UI-thread only.
type settingsDialog struct {
	*walk.Dialog
	*kit
	owner      walk.Form // its Synchronize outlives the dialog, for answers arriving after it closed
	deps       SettingsDeps
	ctl        *settingsCtl
	closed     bool                   // answers arriving now only reach the callbacks
	addAccount bool                   // closed by Add account
	account    string                 // the Privacy section's account
	gen        int                    // per privacy account shown: answers for an earlier one are dropped
	cacheGen   int                    // per cache size measurement: older answers are dropped
	vals       map[string]string      // last saved values, which a failed change returns to
	privacy    map[string]valueWidget // by key
	globals    map[string]valueWidget // by key
	nav        *sectionNav
	pages      [len(sectionNames)]*walk.Composite
	notes       [len(sectionNames)]*label // each section's status line
	acctPick    *choice
	soundChoice *choice
	cacheSize   *label
	keyEdit    *walk.LineEdit
	proxyEdit  *walk.LineEdit
	accounts   *walk.Composite
	addBtn     *button
	lockStored string // the app-lock PIN's hash ("" is off); loaded with the app-global settings
	lockState  *label
	lockBtns   [3]*button // Set PIN, Change PIN, Turn off
}

func (d *settingsDialog) build() error {
	d.account = d.deps.Account
	if accounts := d.deps.Manager.Accounts(); !slices.Contains(accounts, d.account) {
		d.account = ""
		if len(accounts) > 0 {
			d.account = accounts[0]
		}
	}
	if err := errors.Join(d.SetTitle("Settings"), setBox(d.Dialog, walk.NewHBoxLayout(), walk.Margins{}), d.themed(d.Dialog, false),
		d.SetMinMaxSize(walk.Size{Width: settingsW, Height: settingsH}, walk.Size{Width: settingsW, Height: settingsH})); err != nil {
		return err
	}
	nav := &sectionNav{d: d}
	cw, err := d.control(d.Dialog, walk.Size{Width: navW}, walk.AccRolePageTabList, navName, nav.paint, nav.key, nav.hit)
	if err != nil {
		return err
	}
	nav.CustomWidget, d.nav = cw, nav
	content, err := walk.NewComposite(d.Dialog)
	if err != nil {
		return err
	}
	if err := errors.Join(setBox(content, walk.NewVBoxLayout(), walk.Margins{}), d.themed(content, false), fillRest(d.Dialog, content)); err != nil {
		return err
	}
	builders := [...]func(*walk.Composite) error{d.buildPrivacy, d.buildChats, d.buildMedia, d.buildNetwork, d.buildAccounts, d.buildNotify}
	for i, build := range builders {
		pg, err := walk.NewComposite(content)
		if err != nil {
			return err
		}
		d.pages[i] = pg
		l := walk.NewVBoxLayout()
		if err := errors.Join(setBox(pg, l, walk.Margins{HNear: 28, VNear: 16, HFar: 28, VFar: 16}), l.SetSpacing(10), d.themed(pg, false)); err != nil {
			return err
		}
		if _, err := d.newLabel(pg, sectionNames[i], 15, walk.FontBold, toneText, 40); err != nil {
			return err
		}
		if err := build(pg); err != nil {
			return err
		}
		if d.notes[i], err = d.newLabel(pg, "", 9, 0, toneSecondary, 40); err != nil {
			return err
		}
		spacer, err := walk.NewVSpacer(pg)
		if err != nil {
			return err
		}
		if err := fillRest(pg, spacer); err != nil {
			return err
		}
	}
	if err := escCloses(d.Dialog); err != nil {
		return err
	}
	d.show(secPrivacy)
	d.setAccounts(d.deps.Manager.Accounts())
	d.Starting().Attach(func() {
		focusOn(d.nav)
		d.loadGlobals()
		d.loadPrivacy()
		d.measureCache()
	})
	// focus cues follow the user's setting and last input (hidden after a click until a key is used)
	d.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	// cosmetic: Windows builds without dark title bars keep a light one
	logErr("settings title bar", theme.ApplyTitleBar(uintptr(d.Handle()), isDark(d.pal)))
	return nil
}

func (d *settingsDialog) buildPrivacy(pg *walk.Composite) error {
	var err error
	if d.acctPick, err = d.newChoice(pg, "Account", nil); err != nil {
		return err
	}
	d.acctPick.listen(d.switchAccount)
	lastSeen, err := d.newChoice(pg, "Last seen", visibilityOptions)
	if err != nil {
		return err
	}
	photo, err := d.newChoice(pg, "Profile photo", visibilityOptions)
	if err != nil {
		return err
	}
	receipts, err := d.newToggle(pg, "Read receipts", "If off, you don't send or see read receipts (groups always get them).", "all", "none")
	if err != nil {
		return err
	}
	timer, err := d.newChoice(pg, "Default message timer for new chats", timerOptions)
	if err != nil {
		return err
	}
	d.bind(secPrivacy, KeyLastSeen, lastSeen)
	d.bind(secPrivacy, KeyProfilePhoto, photo)
	d.bind(secPrivacy, KeyReadReceipts, receipts)
	d.bind(secPrivacy, KeyDisappearing, timer)
	return d.buildLock(pg)
}

// buildLock adds the App lock row: its state and the buttons that set, change or turn off the PIN.
// The buttons stay hidden until the saved hash is loaded.
func (d *settingsDialog) buildLock(pg *walk.Composite) error {
	row, err := d.newRow(pg, fieldH)
	if err != nil {
		return err
	}
	if d.lockState, err = d.newLabel(row, "App lock", 10, 0, toneText, fieldH); err != nil {
		return err
	}
	if err := fillRest(row, d.lockState); err != nil {
		return err
	}
	for i, b := range []struct {
		text string
		kind int
		w    int
		mode pinMode
	}{{"Set PIN", btnPlain, 110, pinSet}, {"Change PIN", btnPlain, 130, pinChange}, {"Turn off", btnDanger, 110, pinOff}} {
		if d.lockBtns[i], err = d.newButton(row, b.text, b.kind, b.w, func() { d.editLock(b.mode) }); err != nil {
			return err
		}
		d.lockBtns[i].SetVisible(false)
	}
	_, err = d.newLabel(pg, "The lock hides Wisp's window; it doesn't encrypt Wisp's files.", 9, 0, toneSecondary, 20)
	return err
}

// showLock shows the lock state for hash and the buttons that fit it.
func (d *settingsDialog) showLock(hash string) {
	d.lockStored = hash
	state := "App lock: Off"
	if hash != "" {
		state = "App lock: On"
	}
	d.lockState.set(state, toneText)
	d.lockBtns[0].SetVisible(hash == "")
	d.lockBtns[1].SetVisible(hash != "")
	d.lockBtns[2].SetVisible(hash != "")
}

// editLock asks for the PINs mode needs and saves the result.
func (d *settingsDialog) editLock(mode pinMode) {
	title, labels := "Set PIN", []string{"New PIN", "Confirm new PIN"}
	switch mode {
	case pinChange:
		title, labels = "Change PIN", []string{"Current PIN", "New PIN", "Confirm new PIN"}
	case pinOff:
		title, labels = "Turn off app lock", []string{"Current PIN"}
	}
	values, ok, err := askPIN(d.Dialog, d.pal, title, labels...)
	if err != nil {
		d.say(secPrivacy, "Couldn't open the PIN dialog: "+err.Error(), toneError)
		return
	}
	if !ok {
		return
	}
	hash, msg, ok := pinUpdate(d.lockStored, mode, values)
	if !ok {
		d.say(secPrivacy, msg, toneError)
		return
	}
	d.ctl.setGlobal(KeyLockPIN, hash, func(err error) {
		if d.closed {
			return
		}
		if err != nil {
			d.say(secPrivacy, "App lock was not saved: "+err.Error(), toneError)
			return
		}
		d.showLock(hash)
		d.say(secPrivacy, msg, toneSecondary)
	})
}

func (d *settingsDialog) buildChats(pg *walk.Composite) error {
	ghost, err := d.newToggle(pg, "Ghost typing", "Never show others that you are typing.", "true", "false")
	if err != nil {
		return err
	}
	offline, err := d.newToggle(pg, "Appear offline", "Never show as online, even while this app is open.", "true", "false")
	if err != nil {
		return err
	}
	d.bind(secChats, KeyGhostTyping, ghost)
	d.bind(secChats, KeyAppearOffline, offline)
	_, err = d.newLabel(pg, "These apply to this PC only; your phone is unaffected.", 9, 0, toneSecondary, 20)
	return err
}

func (d *settingsDialog) buildMedia(pg *walk.Composite) error {
	auto, err := d.newToggle(pg, "Auto-download images", "Fetch photos as they arrive instead of on a click.", "true", "false")
	if err != nil {
		return err
	}
	d.bind(secMedia, KeyAutoDownloadImages, auto)
	row, err := d.newRow(pg, fieldH)
	if err != nil {
		return err
	}
	if d.cacheSize, err = d.newLabel(row, "Media cache", 10, 0, toneText, fieldH); err != nil {
		return err
	}
	if err := fillRest(row, d.cacheSize); err != nil {
		return err
	}
	if _, err := d.newButton(row, "Clear cache", btnDanger, 120, d.clearCache); err != nil {
		return err
	}
	if _, err := d.newLabel(pg, "GIPHY API key", 10, walk.FontBold, toneText, 22); err != nil {
		return err
	}
	if d.keyEdit, err = d.textRow(pg, "GIPHY API key", "Built-in key", func() {
		d.saveText(secMedia, KeyGiphyAPIKey, d.keyEdit, "GIPHY key saved.")
	}); err != nil {
		return err
	}
	d.keyEdit.SetPasswordMode(true)
	_, err = d.newLabel(pg, "Leave it empty to use the built-in key. It is kept on this PC only.", 9, 0, toneSecondary, 20)
	return err
}

func (d *settingsDialog) buildNetwork(pg *walk.Composite) error {
	if _, err := d.newLabel(pg, "Proxy", 10, walk.FontBold, toneText, 22); err != nil {
		return err
	}
	var err error
	if d.proxyEdit, err = d.textRow(pg, "Proxy address", "socks5://127.0.0.1:1080", func() {
		d.saveText(secNetwork, KeyProxy, d.proxyEdit, "Proxy saved. It takes effect when accounts reconnect.")
	}); err != nil {
		return err
	}
	_, err = d.newLabel(pg, "socks5://, http:// or https:// with host:port, optionally user:password@. "+
		"Leave it empty to connect directly. Changes take effect when accounts reconnect.", 9, 0, toneSecondary, 40)
	return err
}

func (d *settingsDialog) buildAccounts(pg *walk.Composite) error {
	var err error
	if d.accounts, err = walk.NewComposite(pg); err != nil {
		return err
	}
	l := walk.NewVBoxLayout()
	if err := errors.Join(setBox(d.accounts, l, walk.Margins{}), l.SetSpacing(8), d.themed(d.accounts, false)); err != nil {
		return err
	}
	row, err := d.newRow(pg, fieldH)
	if err != nil {
		return err
	}
	if d.addBtn, err = d.newButton(row, "Add account", btnPrimary, 150, func() {
		d.addAccount = true
		d.Accept()
	}); err != nil {
		return err
	}
	spacer, err := walk.NewHSpacer(row)
	if err != nil {
		return err
	}
	if err := fillRest(row, spacer); err != nil {
		return err
	}
	return d.listAccounts()
}

func (d *settingsDialog) buildNotify(pg *walk.Composite) error {
	var soundToggle *toggle
	for _, t := range []struct{ key, name, desc string }{
		{KeyNotify, "Notifications", "Show a Windows notification for new messages."},
		{KeyNotifyPrivate, "Private notifications", "Hide the sender and the message text."},
		{KeyNotifySound, "Notification sound", "Play a sound with each notification."},
	} {
		w, err := d.newToggle(pg, t.name, t.desc, "true", "false")
		if err != nil {
			return err
		}
		if t.key == KeyNotifySound {
			soundToggle = w
		}
		d.bind(secNotify, t.key, w)
	}
	soundChoice, err := d.newChoice(pg, "Sound", soundOptions)
	if err != nil {
		return err
	}
	d.soundChoice = soundChoice
	d.bind(secNotify, KeyNotifySoundName, soundChoice)
	soundChoice.listen(func(v string) {
		d.saveGlobal(secNotify, KeyNotifySoundName, "Sound", v)
		d.previewSound(v)
	})
	if soundToggle != nil {
		soundToggle.listen(func(v string) {
			soundChoice.SetEnabled(v == "true")
			d.saveGlobal(secNotify, KeyNotifySound, "Notification sound", v)
		})
	}
	th, err := d.newChoice(pg, "Theme", themeOptions)
	if err != nil {
		return err
	}
	d.bind(secNotify, KeyTheme, th)
	return nil
}

// bind saves w's changes to key, a per-account or an app-global setting.
func (d *settingsDialog) bind(sec int, key string, w valueWidget) {
	name := settingNames[key]
	if _, ok := privacyKeys[key]; ok {
		d.privacy[key] = w
		w.listen(func(v string) { d.savePrivacy(key, name, v) })
		return
	}
	d.globals[key] = w
	w.listen(func(v string) { d.saveGlobal(sec, key, name, v) })
}

// show switches to section i.
func (d *settingsDialog) show(i int) {
	d.nav.cur = i
	for j, pg := range d.pages { // hide first: two visible pages at once would make walk grow the dialog
		if j != i {
			pg.SetVisible(false)
		}
	}
	d.pages[i].SetVisible(true)
	logErr("settings nav repaint", d.nav.Invalidate())
	announce(d.nav, navName, sectionNames[i])
}

// say puts text on section sec's status line.
func (d *settingsDialog) say(sec int, text string, tone int) { d.notes[sec].set(text, tone) }

// loadGlobals shows the saved app-global values; their controls ignore input until then.
func (d *settingsDialog) loadGlobals() {
	for _, w := range d.globals {
		w.setBusy(true)
	}
	var vals map[string]string
	d.ctl.bg(func() (err error) {
		vals, err = d.ctl.values("")
		return err
	}, func(err error) {
		if d.closed {
			return
		}
		if err != nil { // vals holds the defaults of what could not be read
			for sec := range d.notes {
				d.say(sec, "Couldn't read the saved settings: "+err.Error(), toneError)
			}
		}
		for key, w := range d.globals {
			d.vals[key] = vals[key]
			w.setValue(vals[key])
			w.setBusy(false)
		}
		if d.soundChoice != nil {
			d.soundChoice.SetEnabled(vals[KeyNotifySound] == "true")
		}
		if err == nil { // else the PIN state is unknown: no button, so Set PIN can't replace a PIN unasked
			d.showLock(vals[KeyLockPIN])
		}
		logErr("GIPHY key field", d.keyEdit.SetText(vals[KeyGiphyAPIKey]))
		logErr("proxy field", d.proxyEdit.SetText(vals[KeyProxy]))
	})
}

var pMessageBeep = windows.NewLazySystemDLL("user32.dll").NewProc("MessageBeep")

func (d *settingsDialog) previewSound(v string) {
	if strings.EqualFold(v, "default") {
		_, _, _ = pMessageBeep.Call(0x40) // MB_ICONASTERISK
		return
	}
	if f := notify.SoundFile(v, os.Getenv("WINDIR"), nil); f != "" {
		_ = notify.Play(f)
	}
}

// loadPrivacy shows the privacy account's saved values, then WhatsApp's current ones. Its controls
// ignore input until the saved values are in.
func (d *settingsDialog) loadPrivacy() {
	d.gen++
	gen, id := d.gen, d.account
	if id == "" {
		d.ctl.bg(func() error { return nil }, func(error) { // after the changes queued before it
			if m := d.ctl.takeMissed(); m != "" && !d.closed {
				d.say(secPrivacy, m, toneError)
			}
		})
		return
	}
	for _, w := range d.privacy {
		w.setBusy(true)
	}
	var vals map[string]string
	d.ctl.bg(func() (err error) {
		vals, err = d.ctl.values(id)
		return err
	}, func(err error) {
		if d.closed || gen != d.gen { // the load for the account shown now clears busy
			return
		}
		if err != nil { // vals holds the defaults of what could not be read
			d.say(secPrivacy, "Couldn't read the saved settings: "+err.Error(), toneError)
		}
		for key, w := range d.privacy {
			d.vals[key] = vals[key]
			w.setValue(vals[key])
			w.setBusy(false)
		}
	})
	d.say(secPrivacy, "Checking WhatsApp for changes made elsewhere…", toneSecondary)
	d.ctl.refresh(id, func(vals map[string]string, err error) {
		if d.closed || gen != d.gen {
			return
		}
		// queued behind every earlier change, so one that failed after its dialog closed is known now
		missed := d.ctl.takeMissed()
		if err != nil {
			d.say(secPrivacy, strings.TrimSpace(missed+" Couldn't reach WhatsApp, so these are the values last saved here: "+err.Error()), toneError)
			return
		}
		if missed != "" {
			d.say(secPrivacy, missed, toneError)
		} else {
			d.say(secPrivacy, "", toneSecondary)
		}
		for key, w := range d.privacy {
			d.vals[key] = vals[key]
			if !w.isBusy() { // a change still being saved keeps showing its new value
				w.setValue(vals[key])
			}
		}
	})
}

// setAccounts lists accounts in the Privacy section's picker (shown for two or more) and hides its
// controls when there is none. The section keeps its account while it is still linked.
// shortcut: the picker is one row of segments, so past about four accounts the numbers ellipsize;
// switch to a list or dropdown if users link more.
func (d *settingsDialog) setAccounts(accounts []string) {
	opts := make([]option, len(accounts))
	for i, id := range accounts {
		opts[i] = option{"+" + id, id}
	}
	d.acctPick.opts = opts
	d.acctPick.SetVisible(len(accounts) > 1)
	for _, w := range d.privacy {
		w.SetVisible(len(accounts) > 0)
	}
	if len(accounts) == 0 {
		d.say(secPrivacy, "Link an account to change its privacy settings.", toneSecondary)
	}
	if !slices.Contains(accounts, d.account) {
		d.account = ""
		if len(accounts) > 0 {
			d.account = accounts[0]
		}
		d.loadPrivacy() // also drops answers for the account that went
	}
	d.acctPick.setValue(d.account)
}

// switchAccount shows id's privacy settings.
func (d *settingsDialog) switchAccount(id string) {
	d.account = id
	d.acctPick.setValue(id)
	d.loadPrivacy()
}

// savePrivacy changes the shown account's privacy setting key; the control shows value while it is
// saved and returns to the last saved value if WhatsApp refuses it.
func (d *settingsDialog) savePrivacy(key, name, value string) {
	w, gen := d.privacy[key], d.gen
	w.setBusy(true)
	d.ctl.setPrivacy(d.account, key, value, func(err error) {
		if d.closed || gen != d.gen {
			return
		}
		w.setBusy(false)
		if err == nil || errors.Is(err, errNotMirrored) {
			d.vals[key] = value
		}
		w.setValue(d.vals[key])
		switch {
		case err == nil:
			d.say(secPrivacy, "", toneSecondary)
		case errors.Is(err, errNotMirrored):
			d.say(secPrivacy, name+": "+err.Error(), toneError)
		default:
			d.say(secPrivacy, name+" was not changed: "+err.Error(), toneError)
		}
	})
}

// saveGlobal saves an app-global setting shown in section sec; on failure the control returns to
// the last saved value (and a picked theme is taken back).
func (d *settingsDialog) saveGlobal(sec int, key, name, value string) {
	w := d.globals[key]
	w.setBusy(true)
	d.ctl.setGlobal(key, value, func(err error) {
		if err != nil && key == KeyTheme {
			d.ctl.onTheme(ParseTheme(d.vals[key]))
		}
		if d.closed {
			return
		}
		w.setBusy(false)
		if err != nil {
			w.setValue(d.vals[key])
			d.say(sec, name+" was not saved: "+err.Error(), toneError)
			return
		}
		d.vals[key] = value
		d.say(sec, "", toneSecondary)
	})
}

// saveText saves the text of edit as key; ok is the status line on success.
func (d *settingsDialog) saveText(sec int, key string, edit *walk.LineEdit, ok string) {
	value := strings.TrimSpace(edit.Text())
	d.ctl.setGlobal(key, value, func(err error) {
		if d.closed {
			return
		}
		switch {
		case errors.Is(err, whatsapp.ErrBadProxy):
			d.say(sec, "That is not a proxy address. Use socks5://, http:// or https:// followed by host:port, for example socks5://127.0.0.1:1080.", toneError)
		case err != nil:
			d.say(sec, "Not saved: "+err.Error(), toneError)
		default:
			d.vals[key] = value
			logErr("settings field", edit.SetText(value))
			d.say(sec, ok, toneSecondary)
		}
	})
}

// themeChanged applies a picked theme to the app and to the dialog.
func (d *settingsDialog) themeChanged(m theme.ThemeMode) {
	if d.deps.OnThemeChanged != nil {
		d.deps.OnThemeChanged(m)
	}
	if d.closed {
		return
	}
	logErr("settings theme", d.setPalette(theme.GetPalette(m)))
	logErr("settings title bar", theme.ApplyTitleBar(uintptr(d.Handle()), isDark(d.pal)))
	// DWM recolours a shown caption when it next paints it: repaint it now
	active := uintptr(win.BoolToBOOL(win.GetActiveWindow() == d.Handle()))
	d.SendMessage(win.WM_NCACTIVATE, 1-active, 0)
	d.SendMessage(win.WM_NCACTIVATE, active, 0)
}

// measureCache shows the media cache's size, worked out off the UI thread.
func (d *settingsDialog) measureCache() {
	d.cacheGen++
	gen := d.cacheGen
	d.cacheSize.set("Media cache: working out its size…", toneText)
	go func() {
		n, err := d.deps.Media.GetCacheSizeBytes()
		d.owner.Synchronize(func() {
			if d.closed || gen != d.cacheGen {
				return
			}
			if err != nil {
				d.cacheSize.set("Media cache: size unknown", toneError)
				d.say(secMedia, "Couldn't measure the media cache: "+err.Error(), toneError)
				return
			}
			size := humanSize(uint64(n))
			if n == 0 {
				size = "empty"
			}
			d.cacheSize.set("Media cache: "+size, toneText)
		})
	}()
}

// clearCache deletes the downloaded media once the user confirms, then measures again.
func (d *settingsDialog) clearCache() {
	ok, err := Confirm(d.Dialog, d.pal, "Clear the media cache?",
		"Photos, videos and documents downloaded to this PC are deleted. Your messages stay, and media can be downloaded again while WhatsApp still has it.",
		"Clear cache")
	if err != nil {
		d.say(secMedia, "Couldn't ask before clearing: "+err.Error(), toneError)
	}
	if !ok {
		return
	}
	d.cacheGen++ // a measurement under way is out of date
	d.cacheSize.set("Media cache: clearing…", toneText)
	go func() {
		err := d.deps.Media.ClearMediaCache()
		d.owner.Synchronize(func() {
			if d.deps.OnCacheCleared != nil {
				d.deps.OnCacheCleared()
			}
			if d.closed {
				return
			}
			if err != nil {
				d.say(secMedia, "Some media could not be deleted: "+err.Error(), toneError)
			} else {
				d.say(secMedia, "Media cache cleared.", toneSecondary)
			}
			d.measureCache()
		})
	}()
}

// listAccounts (re)builds the Accounts section's rows: each linked account with a Log out button.
func (d *settingsDialog) listAccounts() error {
	kids := d.accounts.Children()
	rows := make([]walk.Widget, kids.Len())
	for i := range rows {
		rows[i] = kids.At(i)
	}
	for _, w := range rows {
		w.Dispose()
	}
	accounts := d.deps.Manager.Accounts()
	h := max(fieldH, len(accounts)*(fieldH+8)-8) // exactly the rows: spare height would open gaps between them
	if err := d.accounts.SetMinMaxSize(walk.Size{Height: h}, walk.Size{Height: h}); err != nil {
		return err
	}
	if len(accounts) == 0 {
		_, err := d.newLabel(d.accounts, "No linked accounts.", 10, 0, toneSecondary, fieldH)
		return err
	}
	for _, id := range accounts {
		row, err := d.newRow(d.accounts, fieldH)
		if err != nil {
			return err
		}
		var who *walk.CustomWidget
		if who, err = newCanvas(row, func(c *walk.Canvas, _ walk.Rectangle) error {
			r, dpi := who.ClientBoundsPixels(), who.DPI()
			s := scaler(dpi)
			bg, err := brushFor(d.pal.Sidebar)
			if err != nil {
				return err
			}
			cy := r.Height / 2
			return errors.Join(c.FillRectanglePixels(bg, r), drawAvatar(c, dpi, square(s(16), cy, s(32)), "", id, 10),
				drawText(c, dpi, "+"+id, 11, 0, theme.Color(d.pal.Text), walk.Rectangle{X: s(44), Width: r.Width - s(44), Height: r.Height}, leftText))
		}); err != nil {
			return err
		}
		d.canvases = append(d.canvases, who)
		if err := fillRest(row, who); err != nil {
			return err
		}
		logErr("account name", errors.Join(who.Accessibility().SetRole(walk.AccRoleStatictext), who.Accessibility().SetName("+"+id)))
		out, err := d.newButton(row, "Log out", btnDanger, 110, func() { d.logout(id) })
		if err != nil {
			return err
		}
		logErr("log out name", out.Accessibility().SetName("Log out +"+id))
	}
	return nil
}

// logout logs id out once the user confirms, then lists the accounts again.
func (d *settingsDialog) logout(id string) {
	ok, err := Confirm(d.Dialog, d.pal, "Log out +"+id+"?",
		"This PC stops receiving this account's messages. Chats already here are kept. To use it here again, link it with a new QR code.",
		"Log out")
	if err != nil {
		d.say(secAccounts, "Couldn't ask before logging out: "+err.Error(), toneError)
	}
	if !ok {
		return
	}
	d.say(secAccounts, "Logging out +"+id+"…", toneSecondary)
	d.ctl.bg(func() error { return d.deps.Manager.Logout(context.Background(), id) }, func(err error) {
		if d.closed {
			return
		}
		if err != nil {
			d.say(secAccounts, "Couldn't log out +"+id+": "+err.Error(), toneError)
		} else {
			d.say(secAccounts, "+"+id+" is logged out.", toneSecondary)
		}
		if err := d.listAccounts(); err != nil {
			d.say(secAccounts, "Couldn't list the accounts: "+err.Error(), toneError)
		}
		d.setAccounts(d.deps.Manager.Accounts())
		if d.nav.cur == secAccounts { // the focused Log out button is gone
			focusOn(d.addBtn)
		}
	})
}

// serial runs jobs one at a time in the order given, on a goroutine that exists only while there is
// work.
type serial struct {
	mu   sync.Mutex
	jobs []func()
	busy bool
}

func (s *serial) do(job func()) {
	s.mu.Lock()
	s.jobs = append(s.jobs, job)
	start := !s.busy
	s.busy = true
	s.mu.Unlock()
	if start {
		go s.drain()
	}
}

func (s *serial) drain() {
	for {
		s.mu.Lock()
		if len(s.jobs) == 0 {
			s.busy = false
			s.mu.Unlock()
			return
		}
		job := s.jobs[0]
		s.jobs = s.jobs[1:]
		s.mu.Unlock()
		job()
	}
}

// Confirm asks a yes/no question in a small modal dialog over owner, themed in p: the question,
// text, Cancel and the action button. The focus starts on Cancel; Esc cancels. It reports whether
// the action was chosen.
func Confirm(owner walk.Form, p theme.Palette, question, text, action string) (bool, error) {
	dlg, err := walk.NewDialogWithFixedSize(owner)
	if err != nil {
		return false, err
	}
	defer dlg.Dispose()
	k := &kit{pal: p}
	l := walk.NewVBoxLayout()
	if err := errors.Join(dlg.SetTitle(question), setBox(dlg, l, walk.Margins{HNear: 24, VNear: 20, HFar: 24, VFar: 20}), l.SetSpacing(8),
		k.themed(dlg, false), dlg.SetMinMaxSize(walk.Size{Width: 440, Height: 230}, walk.Size{Width: 440, Height: 230})); err != nil {
		return false, err
	}
	if _, err := k.newLabel(dlg, question, 12, walk.FontBold, toneText, 28); err != nil {
		return false, err
	}
	if _, err := k.newLabel(dlg, text, 10, 0, toneSecondary, 60); err != nil {
		return false, err
	}
	spacer, err := walk.NewVSpacer(dlg)
	if err != nil {
		return false, err
	}
	row, err := k.newRow(dlg, fieldH)
	if err != nil {
		return false, err
	}
	gap, err := walk.NewHSpacer(row)
	if err != nil {
		return false, err
	}
	cancel, err := k.newButton(row, "Cancel", btnPlain, 100, dlg.Cancel)
	if err != nil {
		return false, err
	}
	if _, err := k.newButton(row, action, btnDanger, 130, dlg.Accept); err != nil {
		return false, err
	}
	if err := errors.Join(fillRest(dlg, spacer), fillRest(row, gap), escCloses(dlg)); err != nil {
		return false, err
	}
	dlg.Starting().Attach(func() { focusOn(cancel) })
	dlg.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	logErr("confirm title bar", theme.ApplyTitleBar(uintptr(dlg.Handle()), isDark(p)))
	return dlg.Run() == walk.DlgCmdOK, nil
}

// escCloses makes Esc cancel dlg: walk routes Esc (IDCANCEL) only to a cancel button, so it gets a
// hidden one.
func escCloses(dlg *walk.Dialog) error {
	cancel, err := walk.NewPushButton(dlg)
	if err != nil {
		return err
	}
	cancel.SetVisible(false)
	cancel.Clicked().Attach(dlg.Cancel)
	return dlg.SetCancelButton(cancel)
}

// Custom-painted controls of the Settings dialog.

// Text tones.
const (
	toneText = iota
	toneSecondary
	toneError
)

const navName = "Settings sections"

// blockText draws a block of text from the top left, broken into lines.
const blockText = walk.TextLeft | walk.TextTop | walk.TextWordbreak | walk.TextEndEllipsis | walk.TextNoPrefix

// kit makes themed controls and keeps what a palette change must recolour.
type kit struct {
	pal      theme.Palette
	canvases []*walk.CustomWidget
	bgs      []themedBg
	edits    []*walk.LineEdit
}

type themedBg struct {
	w      walk.Window
	header bool // Header colour, else Sidebar
}

func (k *kit) bgHex(header bool) string {
	if header {
		return k.pal.Header
	}
	return k.pal.Sidebar
}

// themed gives w the palette's Sidebar (or Header) background, now and after palette changes.
func (k *kit) themed(w walk.Window, header bool) error {
	k.bgs = append(k.bgs, themedBg{w, header})
	return setBg(w, k.bgHex(header))
}

// setPalette recolours everything k made.
func (k *kit) setPalette(p theme.Palette) error {
	k.pal = p
	k.bgs = slices.DeleteFunc(k.bgs, func(b themedBg) bool { return b.w.IsDisposed() })
	k.canvases = slices.DeleteFunc(k.canvases, func(cw *walk.CustomWidget) bool { return cw.IsDisposed() })
	var errs []error
	for _, b := range k.bgs {
		errs = append(errs, setBg(b.w, k.bgHex(b.header)))
	}
	for _, e := range k.edits {
		e.SetTextColor(theme.Color(p.Text))
	}
	for _, cw := range k.canvases {
		errs = append(errs, cw.Invalidate())
	}
	return errors.Join(errs...)
}

func (k *kit) color(tone int) walk.Color {
	switch {
	case tone == toneSecondary:
		return theme.Color(k.pal.TextSecondary)
	case tone == toneError && isDark(k.pal):
		return walk.RGB(0xf1, 0x5c, 0x6d)
	case tone == toneError:
		return walk.RGB(0xc6, 0x28, 0x28)
	}
	return theme.Color(k.pal.Text)
}

// control adds a focusable custom-painted control of size (1/96 inch; 0 leaves that side free) to
// parent. Keys go to onKey; a left click resolves its target with hit (nil: nothing there), then
// takes the focus, then runs what hit returned.
func (k *kit) control(parent walk.Container, size walk.Size, role walk.AccRole, name string, paint walk.PaintFunc,
	onKey func(walk.Key) bool, hit func(x, y int) func()) (*walk.CustomWidget, error) {
	cw, err := newCanvas(parent, paint)
	if err != nil {
		return nil, err
	}
	k.canvases = append(k.canvases, cw)
	if err := errors.Join(cw.SetMinMaxSize(size, size), focusable(cw, role, name, onKey)); err != nil {
		return nil, err
	}
	cw.MouseDown().Attach(func(x, y int, b walk.MouseButton) {
		if b != walk.LeftButton {
			return
		}
		act := hit(x, y)
		focusOn(cw)
		if act != nil {
			act()
		}
	})
	return cw, nil
}

// newRow adds a horizontal row h (1/96 inch) high.
func (k *kit) newRow(parent walk.Container, h int) (*walk.Composite, error) {
	row, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	l := walk.NewHBoxLayout()
	return row, errors.Join(setBox(row, l, walk.Margins{}), l.SetSpacing(8), l.SetAlignment(walk.AlignHNearVCenter), k.themed(row, false),
		row.SetMinMaxSize(walk.Size{Height: h}, walk.Size{Height: h}))
}

// textRow adds a one-line text field named name, showing cue while empty, and a Save button; Save
// or Enter in the field runs save.
func (k *kit) textRow(parent walk.Container, name, cue string, save func()) (*walk.LineEdit, error) {
	row, err := k.newRow(parent, fieldH)
	if err != nil {
		return nil, err
	}
	pill, err := walk.NewComposite(row)
	if err != nil {
		return nil, err
	}
	l := walk.NewHBoxLayout()
	if err := errors.Join(setBox(pill, l, walk.Margins{HNear: 12, HFar: 12}), l.SetAlignment(walk.AlignHNearVCenter),
		k.themed(pill, true), fillRest(row, pill)); err != nil {
		return nil, err
	}
	if _, err := walk.NewVSpacerFixed(pill, fieldH); err != nil { // the pill is as high as the Save button
		return nil, err
	}
	edit, err := walk.NewLineEdit(pill)
	if err != nil {
		return nil, err
	}
	edit.SetFont(font(10, 0))
	edit.SetTextColor(theme.Color(k.pal.Text))
	k.edits = append(k.edits, edit)
	if err := errors.Join(noBorder(edit), cueBanner(edit, cue), edit.Accessibility().SetName(name), k.themed(edit, true)); err != nil {
		return nil, err
	}
	edit.KeyDown().Attach(func(key walk.Key) {
		if key == walk.KeyReturn {
			save()
		}
	})
	btn, err := k.newButton(row, "Save", btnPlain, 90, save)
	if err != nil {
		return nil, err
	}
	return edit, btn.Accessibility().SetName("Save " + name)
}

// valueWidget is a control showing one setting's value.
type valueWidget interface {
	walk.Widget
	setValue(v string)
	setBusy(busy bool) // busy: its value is loading or being saved, and it ignores input
	isBusy() bool
	listen(fn func(value string)) // fn gets each value the user picks
}

// label is a block of text, wrapped into lines; its text and tone can change.
type label struct {
	*walk.CustomWidget
	k     *kit
	text  string
	pt    int
	style walk.FontStyle
	tone  int
	mid   bool // centred in its width, one line
}

// newLabel adds a label h (1/96 inch) high.
func (k *kit) newLabel(parent walk.Container, text string, pt int, style walk.FontStyle, tone, h int) (*label, error) {
	l := &label{k: k, text: text, pt: pt, style: style, tone: tone}
	cw, err := newCanvas(parent, l.paint)
	if err != nil {
		return nil, err
	}
	l.CustomWidget = cw
	k.canvases = append(k.canvases, cw)
	return l, errors.Join(cw.SetMinMaxSize(walk.Size{Height: h}, walk.Size{Height: h}),
		cw.Accessibility().SetRole(walk.AccRoleStatictext), cw.Accessibility().SetName(text))
}

func (l *label) set(text string, tone int) {
	l.text, l.tone = text, tone
	logErr("label repaint", l.Invalidate())
	logErr("label name", l.Accessibility().SetName(text))
}

func (l *label) paint(c *walk.Canvas, _ walk.Rectangle) error {
	r := l.ClientBoundsPixels()
	bg, err := brushFor(l.k.pal.Sidebar)
	if err != nil {
		return err
	}
	format := blockText
	if r.Height <= l.IntFrom96DPI(fieldH) { // one line, centred: level with a button beside it
		format = leftText
		if l.mid {
			format = centerText
		}
	}
	return errors.Join(c.FillRectanglePixels(bg, r), drawText(c, l.DPI(), l.text, l.pt, l.style, l.k.color(l.tone), r, format))
}

// toggle is a switch row: a label with a description under it, and the switch on the right. Space,
// Enter or a click flips it.
type toggle struct {
	*walk.CustomWidget
	k             *kit
	label, desc   string
	onVal, offVal string // the setting's values for on and off
	on, busy      bool
	onChange      func(string)
}

func (k *kit) newToggle(parent walk.Container, label, desc, onVal, offVal string) (*toggle, error) {
	t := &toggle{k: k, label: label, desc: desc, onVal: onVal, offVal: offVal}
	cw, err := k.control(parent, walk.Size{Height: toggleH}, walk.AccRoleCheckbutton, label, t.paint, func(key walk.Key) bool {
		if key != walk.KeySpace && key != walk.KeyReturn {
			return false
		}
		t.flip()
		return true
	}, func(int, int) func() { return t.flip })
	if err != nil {
		return nil, err
	}
	t.CustomWidget = cw
	return t, cw.Accessibility().SetDescription(desc)
}

func (t *toggle) setValue(v string)      { t.on = v == t.onVal; t.changed() }
func (t *toggle) setBusy(busy bool)      { t.busy = busy; t.changed() }
func (t *toggle) isBusy() bool           { return t.busy }
func (t *toggle) listen(fn func(string)) { t.onChange = fn }

func (t *toggle) flip() {
	if t.busy {
		return
	}
	t.on = !t.on
	t.changed()
	if t.onChange != nil {
		v := t.offVal
		if t.on {
			v = t.onVal
		}
		t.onChange(v)
	}
}

func (t *toggle) changed() {
	logErr("toggle repaint", t.Invalidate())
	state := "off"
	if t.on {
		state = "on"
	}
	if t.busy {
		state += ", saving"
	}
	announce(t, t.label, state)
}

func (t *toggle) paint(c *walk.Canvas, _ walk.Rectangle) error {
	p, r, dpi := t.k.pal, t.ClientBoundsPixels(), t.DPI()
	s := scaler(dpi)
	bg, err := brushFor(p.Sidebar)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, r); err != nil {
		return err
	}
	sw := walk.Rectangle{X: r.Width - s(4) - s(40), Y: (r.Height - s(22)) / 2, Width: s(40), Height: s(22)}
	track, fg := theme.Color(p.TextSecondary), theme.Color(p.Text)
	if t.on && !t.busy {
		track = theme.Color(p.Accent)
	}
	if t.busy {
		fg = theme.Color(p.TextSecondary)
	}
	knob := sw.Height - s(6)
	kx := sw.X + s(3) + knob/2
	if t.on {
		kx = sw.X + sw.Width - s(3) - knob/2
	}
	textW := sw.X - s(16)
	if focusCues(t) { // a ring in the text colour around the switch
		ring := inset(sw, -s(4))
		if err := errors.Join(icons.Fill(c, ring, theme.Color(p.Text)), icons.Fill(c, inset(ring, s(2)), theme.Color(p.Sidebar))); err != nil {
			return err
		}
	}
	return errors.Join(
		drawText(c, dpi, t.label, 10, 0, fg, walk.Rectangle{Y: s(7), Width: textW, Height: s(22)}, leftText),
		drawText(c, dpi, t.desc, 9, 0, theme.Color(p.TextSecondary), walk.Rectangle{Y: s(29), Width: textW, Height: s(20)}, leftText),
		icons.Fill(c, sw, track), icons.Fill(c, square(kx, sw.Y+sw.Height/2, knob), walk.RGB(255, 255, 255)))
}

// choice is a labelled row of segments, one per option. Left/Right (Up/Down) move the keyboard
// cursor, Space or Enter pick the option under it, a click picks one.
type choice struct {
	*walk.CustomWidget
	k        *kit
	label    string
	opts     []option
	sel, cur int // the picked option (-1 for none) and the keyboard cursor
	busy     bool
	onChange func(string)
}

func (k *kit) newChoice(parent walk.Container, label string, opts []option) (*choice, error) {
	ch := &choice{k: k, label: label, opts: opts, sel: -1}
	cw, err := k.control(parent, walk.Size{Height: choiceH}, walk.AccRoleList, label, ch.paint, ch.key, ch.hit)
	if err != nil {
		return nil, err
	}
	ch.CustomWidget = cw
	cw.FocusedChanged().Attach(func() {
		if cw.Focused() { // the cursor starts on the picked option
			ch.cur = max(ch.sel, 0)
			ch.changed()
		}
	})
	return ch, nil
}

func (ch *choice) setBusy(busy bool)      { ch.busy = busy; ch.changed() }
func (ch *choice) isBusy() bool           { return ch.busy }
func (ch *choice) listen(fn func(string)) { ch.onChange = fn }

func (ch *choice) setValue(v string) {
	ch.sel = slices.IndexFunc(ch.opts, func(o option) bool { return o.value == v })
	ch.cur = max(ch.sel, 0)
	ch.changed()
}

func (ch *choice) changed() {
	logErr("choice repaint", ch.Invalidate())
	if ch.cur >= len(ch.opts) {
		return
	}
	item := ch.opts[ch.cur].label
	if ch.cur == ch.sel {
		item += ", selected"
	}
	if ch.busy {
		item += ", saving"
	}
	announce(ch, ch.label, item)
}

func (ch *choice) pick(i int) {
	if !ch.Enabled() || ch.busy || i < 0 || i >= len(ch.opts) || i == ch.sel {
		return
	}
	ch.sel = i
	ch.changed()
	if ch.onChange != nil {
		ch.onChange(ch.opts[i].value)
	}
}

func (ch *choice) key(k walk.Key) bool {
	if !ch.Enabled() {
		return false
	}
	if k == walk.KeySpace || k == walk.KeyReturn {
		ch.pick(ch.cur)
		return true
	}
	next, ok := navigate(ch.cur, len(ch.opts), 1, k, false)
	if ok && next >= 0 {
		ch.cur = next
		ch.changed()
	}
	return ok
}

// segments are the options' rectangles, side by side under the label.
func (ch *choice) segments() []walk.Rectangle {
	r, s := ch.ClientBoundsPixels(), scaler(ch.DPI())
	top := s(26)
	out := make([]walk.Rectangle, len(ch.opts))
	for i := range out {
		x0, x1 := r.Width*i/len(out), r.Width*(i+1)/len(out)
		out[i] = walk.Rectangle{X: x0, Y: top, Width: x1 - x0, Height: r.Height - top - s(2)}
	}
	return out
}

func (ch *choice) hit(x, y int) func() {
	if !ch.Enabled() {
		return nil
	}
	for i, seg := range ch.segments() {
		if x >= seg.X && x < seg.X+seg.Width && y >= seg.Y && y < seg.Y+seg.Height {
			return func() {
				ch.cur = i
				ch.changed()
				ch.pick(i)
			}
		}
	}
	return nil
}

func (ch *choice) paint(c *walk.Canvas, _ walk.Rectangle) error {
	p, r, dpi := ch.k.pal, ch.ClientBoundsPixels(), ch.DPI()
	s := scaler(dpi)
	bg, err := brushFor(p.Sidebar)
	if err != nil {
		return err
	}
	labelCol := theme.Color(p.Text)
	if !ch.Enabled() {
		labelCol = theme.Color(p.TextSecondary)
	}
	if err := errors.Join(c.FillRectanglePixels(bg, r),
		drawText(c, dpi, ch.label, 10, 0, labelCol, walk.Rectangle{Width: r.Width, Height: s(22)}, leftText)); err != nil {
		return err
	}
	segs := ch.segments()
	if len(segs) == 0 {
		return nil
	}
	bar := walk.Rectangle{Y: segs[0].Y, Width: r.Width, Height: segs[0].Height}
	if err := icons.FillRounded(c, bar, s(8), theme.Color(p.Header)); err != nil {
		return err
	}
	focused := focusCues(ch) && ch.Enabled()
	for i, seg := range segs {
		in := inset(seg, s(3))
		fill, fg, style := theme.Color(p.Header), theme.Color(p.Text), walk.FontStyle(0)
		if !ch.Enabled() {
			fg = theme.Color(p.TextSecondary)
			if i == ch.sel {
				fill = theme.Color(p.Header)
			}
		} else if i == ch.sel {
			fill, fg, style = theme.Color(p.Accent), walk.RGB(255, 255, 255), walk.FontBold
			if ch.busy {
				fill = theme.Color(p.TextSecondary)
			}
		}
		if focused && i == ch.cur { // a ring in the text colour, the segment inside it
			if err := icons.FillRounded(c, in, s(6), theme.Color(p.Text)); err != nil {
				return err
			}
			in = inset(in, s(2))
		}
		if err := errors.Join(icons.FillRounded(c, in, s(5), fill), drawText(c, dpi, ch.opts[i].label, 9, style, fg, in, centerText)); err != nil {
			return err
		}
	}
	return nil
}

// Button kinds.
const (
	btnPlain = iota
	btnPrimary
	btnDanger
	btnLink // accent text, underlined, no pill
)

// button is a pill with a text; a click, Enter or Space runs act.
type button struct {
	*walk.CustomWidget
	k    *kit
	text string
	kind int
}

// newButton adds a button width (1/96 inch) wide.
func (k *kit) newButton(parent walk.Container, text string, kind, width int, act func()) (*button, error) {
	b := &button{k: k, text: text, kind: kind}
	cw, err := k.control(parent, walk.Size{Width: width, Height: fieldH}, walk.AccRolePushbutton, text, b.paint, func(key walk.Key) bool {
		if key == walk.KeySpace || key == walk.KeyReturn {
			act()
			return true
		}
		return false
	}, func(int, int) func() { return act })
	if err != nil {
		return nil, err
	}
	b.CustomWidget = cw
	return b, nil
}

func (b *button) paint(c *walk.Canvas, _ walk.Rectangle) error {
	p, r, dpi := b.k.pal, b.ClientBoundsPixels(), b.DPI()
	s := scaler(dpi)
	bg, err := brushFor(p.Sidebar)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, r); err != nil {
		return err
	}
	fill, fg, style := theme.Color(p.Header), theme.Color(p.Text), walk.FontBold
	switch b.kind {
	case btnPrimary:
		fill, fg = theme.Color(p.Accent), walk.RGB(255, 255, 255)
	case btnDanger:
		fg = b.k.color(toneError)
	case btnLink:
		fill, fg, style = theme.Color(p.Sidebar), theme.Color(p.Accent), walk.FontUnderline
	}
	pill := r
	if focusCues(b) {
		if err := icons.Fill(c, pill, theme.Color(p.Text)); err != nil {
			return err
		}
		pill = inset(pill, s(2))
	}
	return errors.Join(icons.Fill(c, pill, fill), drawText(c, dpi, b.text, 10, style, fg, pill, centerText))
}

func inset(r walk.Rectangle, d int) walk.Rectangle {
	return walk.Rectangle{X: r.X + d, Y: r.Y + d, Width: r.Width - 2*d, Height: r.Height - 2*d}
}

// sectionNav is the Settings dialog's section list: Up/Down/Home/End or a click switch sections.
type sectionNav struct {
	*walk.CustomWidget
	d   *settingsDialog
	cur int
}

func (n *sectionNav) rows() span {
	return span{pad: n.IntFrom96DPI(16), size: n.IntFrom96DPI(navRowH), n: len(sectionNames)}
}

func (n *sectionNav) key(k walk.Key) bool {
	next, ok := navigate(n.cur, len(sectionNames), 1, k, true)
	if ok && next >= 0 {
		n.d.show(next)
	}
	return ok
}

func (n *sectionNav) hit(_, y int) func() {
	if i := n.rows().at(y); i >= 0 {
		return func() { n.d.show(i) }
	}
	return nil
}

func (n *sectionNav) paint(c *walk.Canvas, _ walk.Rectangle) error {
	p, r, dpi := n.d.pal, n.ClientBoundsPixels(), n.DPI()
	s := scaler(dpi)
	bg, err := brushFor(p.Header)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, r); err != nil {
		return err
	}
	rows, focused := n.rows(), focusCues(n)
	for i, name := range sectionNames {
		row := walk.Rectangle{X: s(10), Y: rows.pad + i*rows.size, Width: r.Width - s(20), Height: rows.size - s(4)}
		style := walk.FontStyle(0)
		if i == n.cur {
			style = walk.FontBold
			if err := errors.Join(icons.FillRounded(c, row, s(8), theme.Color(p.Sidebar)),
				icons.Fill(c, walk.Rectangle{X: row.X + s(6), Y: row.Y + row.Height/2 - s(10), Width: s(4), Height: s(20)}, theme.Color(p.Accent))); err != nil {
				return err
			}
		}
		if err := drawText(c, dpi, name, 10, style, theme.Color(p.Text), walk.Rectangle{X: row.X + s(20), Y: row.Y, Width: row.Width - s(26), Height: row.Height}, leftText); err != nil {
			return err
		}
		if focused && i == n.cur {
			if err := focusRect(c, row, max(1, s(2)), p.Text); err != nil {
				return err
			}
		}
	}
	return nil
}
