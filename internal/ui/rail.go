// Package ui holds the main window's custom-painted widgets and the plain models behind them.
//
// Everything here is UI-thread only: whatsmeow events arrive on background goroutines and must be
// marshalled onto the UI thread (walk's Synchronize) before they reach a model or widget.
package ui

import (
	"errors"
	"log"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/lxn/walk"

	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

// Widths in 1/96 inch, scaled to the window DPI by walk's layout.
const (
	railWidth    = 56
	sidebarWidth = 280
)

// RailModel is the rail's state: accounts in display order, the active one, per-account unread
// totals and connection state. UI-thread only.
type RailModel struct {
	accounts  []string
	active    string
	unread    map[string]int
	connected map[string]bool
	listeners []func(accountID string)
	redraw    func() // set by the Rail widget
}

func NewRailModel() *RailModel {
	return &RailModel{unread: map[string]int{}, connected: map[string]bool{}}
}

func (m *RailModel) changed() {
	if m.redraw != nil {
		m.redraw()
	}
}

// OnActiveChanged registers fn, called with the new active account ("" when none is left) each time
// the active account changes.
func (m *RailModel) OnActiveChanged(fn func(accountID string)) {
	m.listeners = append(m.listeners, fn)
}

// SetAccounts replaces the accounts, kept in the given order. The active account stays when still
// listed; otherwise the first account (or none) becomes active and the listeners fire.
func (m *RailModel) SetAccounts(ids []string) {
	m.accounts = slices.Clone(ids)
	gone := func(id string) bool { return !slices.Contains(ids, id) }
	maps.DeleteFunc(m.unread, func(id string, _ int) bool { return gone(id) })
	maps.DeleteFunc(m.connected, func(id string, _ bool) bool { return gone(id) })
	if gone(m.active) {
		next := ""
		if len(ids) > 0 {
			next = ids[0]
		}
		m.switchTo(next)
	}
	m.changed()
}

// SetActive makes id the active account. Listeners fire only when the active account actually
// changes; an id that is not an account is ignored.
func (m *RailModel) SetActive(id string) {
	if slices.Contains(m.accounts, id) {
		m.switchTo(id)
		m.changed()
	}
}

func (m *RailModel) switchTo(id string) {
	if id == m.active {
		return
	}
	m.active = id
	for _, fn := range m.listeners {
		fn(id)
	}
}

func (m *RailModel) Accounts() []string { return slices.Clone(m.accounts) }
func (m *RailModel) Active() string     { return m.active }
func (m *RailModel) Unread(id string) int {
	return m.unread[id]
}
func (m *RailModel) Connected(id string) bool { return m.connected[id] }

// SetUnread sets the unread total shown on id's avatar.
func (m *RailModel) SetUnread(id string, n int) {
	if m.unread[id] != n {
		m.unread[id] = n
		m.changed()
	}
}

// SetConnected sets whether id's status dot is green (connected) or grey.
func (m *RailModel) SetConnected(id string, ok bool) {
	if m.connected[id] != ok {
		m.connected[id] = ok
		m.changed()
	}
}

// Nav is a rail navigation button.
type Nav int

const (
	NavChats Nav = iota
	NavStatus
	NavSettings
)

var navIcons = [...]icons.Icon{NavChats: icons.IconChat, NavStatus: icons.IconStatus, NavSettings: icons.IconSettings}
var navNames = [...]string{NavChats: "Chats", NavStatus: "Status", NavSettings: "Settings"}

// Rail is the 56px account and navigation rail: account avatars (active bar, status dot, unread
// badge), the add-account button, Chats and Status, and Settings at the bottom. Clicking an avatar
// calls Model().SetActive. UI-thread only.
type Rail struct {
	*walk.CustomWidget
	model *RailModel
	pal   theme.Palette
	nav   Nav // highlighted: NavChats or NavStatus
	focus int // keyboard focus: index into targets(), -1 for none
	onAdd func()
	onNav func(Nav)
}

const railName = "Accounts and navigation"

// NewRail creates the rail in parent. It is a tab stop: Up/Down move its focus ring (wrapping),
// Enter or Space activate the focused item.
func NewRail(parent walk.Container, p theme.Palette) (*Rail, error) {
	r := &Rail{model: NewRailModel(), pal: p, focus: -1}
	cw, err := newCanvas(parent, r.paint)
	if err != nil {
		return nil, err
	}
	r.CustomWidget = cw
	if err := errors.Join(cw.SetMinMaxSize(walk.Size{Width: railWidth}, walk.Size{Width: railWidth}),
		focusable(cw, walk.AccRoleToolbar, railName, r.key)); err != nil {
		cw.Dispose()
		return nil, err
	}
	r.model.redraw = func() { logErr("rail repaint", cw.Invalidate()) }
	cw.MouseDown().Attach(r.click)
	cw.FocusedChanged().Attach(func() {
		if cw.Focused() && r.focus < 0 { // start on the active account
			r.focusOn(max(0, slices.Index(r.model.accounts, r.model.active)))
		}
	})
	tipOnHover(cw, func(x, y int) string {
		if it, ok := r.hit(x, y); ok {
			return itemName(it)
		}
		return ""
	})
	return r, nil
}

// itemName is a rail item's tooltip.
func itemName(it railItem) string {
	switch it.kind {
	case railAccount:
		return "+" + it.account
	case railAdd:
		return "Add account"
	}
	return navNames[it.nav]
}

// describe is a rail item's accessible name, with the account's state.
func (r *Rail) describe(it railItem) string {
	name := itemName(it)
	if it.kind != railAccount {
		return name
	}
	if it.account == r.model.active {
		name += ", active"
	}
	if r.model.connected[it.account] {
		name += ", connected"
	} else {
		name += ", offline"
	}
	if n := r.model.unread[it.account]; n > 0 {
		name += ", " + strconv.Itoa(n) + " unread"
	}
	return name
}

func (r *Rail) Model() *RailModel { return r.model }

// OnAddAccount sets the handler of the + button (start QR pairing).
func (r *Rail) OnAddAccount(fn func()) { r.onAdd = fn }

// OnNav sets the handler of the Chats, Status and Settings buttons.
func (r *Rail) OnNav(fn func(Nav)) { r.onNav = fn }

// SetPalette repaints the rail in p.
func (r *Rail) SetPalette(p theme.Palette) error {
	r.pal = p
	return r.Invalidate()
}

const (
	railAccount = iota
	railAdd
	railDivider
	railNav
)

type railItem struct {
	kind    int
	account string
	nav     Nav
	r       walk.Rectangle // pixels
}

// items lays the rail out for the current DPI and height; paint and hit-testing share it.
// shortcut: no scrolling, so about 8 accounts fit a 720px-high window; add scrolling if users run more.
func (r *Rail) items() []railItem {
	s := scaler(r.DPI())
	b := r.ClientBoundsPixels()
	var out []railItem
	y := s(8)
	add := func(kind int, h int, account string, nav Nav) {
		out = append(out, railItem{kind: kind, account: account, nav: nav, r: walk.Rectangle{Y: y, Width: b.Width, Height: h}})
		y += h
	}
	for _, id := range r.model.accounts {
		add(railAccount, s(52), id, 0)
	}
	add(railAdd, s(52), "", 0)
	add(railDivider, s(17), "", 0)
	add(railNav, s(52), "", NavChats)
	add(railNav, s(52), "", NavStatus)
	y = max(y, b.Height-s(60))
	add(railNav, s(52), "", NavSettings)
	return out
}

// targets are the items keyboard focus moves over: all but the divider.
func (r *Rail) targets() []railItem {
	return slices.DeleteFunc(r.items(), func(it railItem) bool { return it.kind == railDivider })
}

func (r *Rail) hit(x, y int) (railItem, bool) {
	for _, it := range r.targets() {
		if y >= it.r.Y && y < it.r.Y+it.r.Height && x >= 0 && x < it.r.Width {
			return it, true
		}
	}
	return railItem{}, false
}

// focusOn moves the focus ring to target i.
func (r *Rail) focusOn(i int) {
	ts := r.targets()
	r.focus = min(i, len(ts)-1)
	logErr("rail repaint", r.Invalidate())
	if r.focus >= 0 {
		announce(r, railName, r.describe(ts[r.focus]))
	}
}

func (r *Rail) key(k walk.Key) bool {
	ts := r.targets()
	if k == walk.KeyReturn || k == walk.KeySpace {
		if r.focus >= 0 && r.focus < len(ts) {
			r.activate(ts[r.focus])
		}
		return true
	}
	next, ok := navigate(r.focus, len(ts), 1, k, true)
	if ok && next >= 0 {
		r.focusOn(next)
	}
	return ok
}

func (r *Rail) click(x, y int, button walk.MouseButton) {
	if button != walk.LeftButton {
		return
	}
	// the clicked item becomes the focused, announced one before focus arrives (which would
	// otherwise start on the active account)
	it, ok := r.hit(x, y)
	if ok {
		r.focusOn(slices.IndexFunc(r.targets(), func(t railItem) bool { return t.r == it.r }))
	}
	focusOn(r)
	if ok {
		r.activate(it)
	}
}

// activate does what clicking it does.
func (r *Rail) activate(it railItem) {
	switch it.kind {
	case railAccount:
		r.model.SetActive(it.account)
	case railAdd:
		if r.onAdd != nil {
			r.onAdd()
		}
	case railNav:
		if it.nav != NavSettings && it.nav != r.nav {
			r.nav = it.nav
			logErr("rail repaint", r.Invalidate())
		}
		if r.onNav != nil {
			r.onNav(it.nav)
		}
	}
}

// The rail is dark in both themes, so its foreground comes from the dark palette.
var railFg = theme.GetPalette(theme.ThemeDark)

func (r *Rail) paint(c *walk.Canvas, _ walk.Rectangle) error {
	dpi := r.DPI()
	s := scaler(dpi)
	bg, err := brushFor(r.pal.Rail)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, r.ClientBoundsPixels()); err != nil {
		return err
	}
	accent := theme.Color(r.pal.Accent)
	focused, target := focusCues(r), -1
	for _, it := range r.items() {
		cx, cy := it.r.X+it.r.Width/2, it.r.Y+it.r.Height/2
		var err error
		if it.kind != railDivider {
			if target++; focused && target == r.focus {
				err = focusRing(c, cx, cy, s(48), s(2), theme.Color(railFg.Text), theme.Color(r.pal.Rail))
			}
		}
		switch {
		case err != nil:
		case it.kind == railAccount:
			err = r.paintAccount(c, dpi, it.account, square(cx, cy, s(36)), s)
		case it.kind == railAdd:
			if err = icons.Fill(c, square(cx, cy, s(36)), theme.Color(railFg.Header)); err == nil {
				err = icons.Draw(c, icons.IconAdd, square(cx, cy, s(20)), theme.Color(railFg.TextSecondary))
			}
		case it.kind == railDivider:
			var line *walk.SolidColorBrush
			if line, err = brushFor(railFg.Header); err == nil {
				err = c.FillRectanglePixels(line, walk.Rectangle{X: s(14), Y: cy, Width: it.r.Width - s(28), Height: max(1, s(1))})
			}
		case it.kind == railNav:
			fg := theme.Color(railFg.TextSecondary)
			if it.nav == r.nav {
				fg = theme.Color(railFg.Text)
				err = icons.Fill(c, square(cx, cy, s(40)), theme.Color(railFg.Header))
			}
			if err == nil {
				err = icons.Draw(c, navIcons[it.nav], square(cx, cy, s(24)), fg)
			}
		}
		if err != nil {
			return err
		}
		if it.kind == railAccount && it.account == r.model.active {
			// accent bar on the left edge, rounded only on the right
			if err := icons.Fill(c, walk.Rectangle{X: -s(3), Y: cy - s(12), Width: s(7), Height: s(24)}, accent); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Rail) paintAccount(c *walk.Canvas, dpi int, id string, av walk.Rectangle, s func(int) int) error {
	if err := drawAvatar(c, dpi, av, "", id, 10); err != nil {
		return err
	}
	dot := theme.Color(railFg.TextSecondary)
	if r.model.connected[id] {
		dot = theme.Color(r.pal.Accent)
	}
	cx, cy := av.X+av.Width-s(5), av.Y+av.Height-s(5)
	if err := icons.Fill(c, square(cx, cy, s(14)), theme.Color(r.pal.Rail)); err != nil {
		return err
	}
	if err := icons.Fill(c, square(cx, cy, s(10)), dot); err != nil {
		return err
	}
	n := r.model.unread[id]
	if n <= 0 {
		return nil
	}
	label := strconv.Itoa(n)
	if n > 9 {
		label = "9+" // the rail is narrow
	}
	return drawBadge(c, dpi, av.X+av.Width+s(6), av.Y-s(4), s(18), label, theme.Color(r.pal.Accent), theme.Color(r.pal.Sidebar), 7)
}

// Shared painting helpers.

const (
	leftText   = walk.TextLeft | walk.TextVCenter | walk.TextSingleLine | walk.TextEndEllipsis | walk.TextNoPrefix
	rightText  = walk.TextRight | walk.TextVCenter | walk.TextSingleLine | walk.TextNoPrefix
	centerText = walk.TextCenter | walk.TextVCenter | walk.TextSingleLine | walk.TextEndEllipsis | walk.TextNoPrefix
)

// newCanvas creates a double-buffered custom-painted widget that repaints when resized.
func newCanvas(parent walk.Container, paint walk.PaintFunc) (*walk.CustomWidget, error) {
	cw, err := walk.NewCustomWidgetPixels(parent, 0, paint)
	if err != nil {
		return nil, err
	}
	cw.SetPaintMode(walk.PaintBuffered)
	cw.SetInvalidatesOnResize(true)
	return cw, nil
}

// scaler converts 1/96 inch to pixels at dpi.
func scaler(dpi int) func(int) int {
	return func(v int) int { return walk.IntFrom96DPI(v, dpi) }
}

func square(cx, cy, d int) walk.Rectangle {
	return walk.Rectangle{X: cx - d/2, Y: cy - d/2, Width: d, Height: d}
}

// font returns the Segoe UI font of pt points; walk caches fonts, so this is a map lookup.
func font(pt int, style walk.FontStyle) *walk.Font {
	f, err := walk.NewFont("Segoe UI", pt, style)
	if err != nil {
		panic(err) // NewFont fails only for an invalid style constant
	}
	return f
}

// brushes holds one paint brush per palette colour; never disposed, the palettes bound the set.
// Never pass one to SetBackground: walk deletes a background brush once no window uses it (setBg).
var brushes = map[string]*walk.SolidColorBrush{}

func brushFor(hex string) (*walk.SolidColorBrush, error) {
	if b, ok := brushes[hex]; ok {
		return b, nil
	}
	b, err := walk.NewSolidColorBrush(theme.Color(hex))
	if err != nil {
		return nil, err
	}
	brushes[hex] = b
	return b, nil
}

// setBg gives w a background brush of its own in hex; walk deletes it when w drops it.
func setBg(w walk.Window, hex string) error {
	b, err := walk.NewSolidColorBrush(theme.Color(hex))
	if err != nil {
		return err
	}
	w.SetBackground(b)
	return nil
}

// avatarColors are the initials-avatar backgrounds; white initials stay legible on each.
var avatarColors = [...]walk.Color{
	walk.RGB(0xc7, 0x5a, 0x62), walk.RGB(0xd0, 0x7a, 0x3e), walk.RGB(0x8a, 0x76, 0xd6), walk.RGB(0x4f, 0x9a, 0x3d),
	walk.RGB(0x2f, 0x9a, 0x9d), walk.RGB(0x4a, 0x8f, 0xc4), walk.RGB(0xc8, 0x5a, 0x92), walk.RGB(0xa8, 0x82, 0x1e),
}

// avatarColor picks a colour from jid with FNV-1a, so a contact keeps its colour everywhere.
func avatarColor(jid string) walk.Color {
	h := uint32(2166136261)
	for i := 0; i < len(jid); i++ {
		h = (h ^ uint32(jid[i])) * 16777619
	}
	return avatarColors[h%uint32(len(avatarColors))]
}

// initials are the first letters of the first two words of name, else the last two digits of jid.
func initials(name, jid string) string {
	var out []rune
	for _, w := range strings.Fields(name) {
		for _, r := range w {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				out = append(out, unicode.ToUpper(r))
				break
			}
		}
		if len(out) == 2 {
			break
		}
	}
	if len(out) > 0 {
		return string(out)
	}
	user, _, _ := strings.Cut(jid, "@")
	return user[max(0, len(user)-2):]
}

func drawAvatar(c *walk.Canvas, dpi int, r walk.Rectangle, name, jid string, pt int) error {
	if !strings.Contains(jid, "@") && jid != "" {
		jid = jid + "@s.whatsapp.net"
	}
	if av := getAvatars(); av != nil && jid != "" {
		if bmp := av.Get(jid, dpi); bmp != nil {
			return c.DrawImageStretchedPixels(bmp, r)
		}
	}
	if err := icons.Fill(c, r, avatarColor(jid)); err != nil {
		return err
	}
	return drawText(c, dpi, initials(name, jid), pt, 0, walk.RGB(255, 255, 255), r, centerText)
}

// drawBadge paints an accent pill of height h holding label, right-aligned at right.
func drawBadge(c *walk.Canvas, dpi, right, top, h int, label string, fill, text walk.Color, pt int) error {
	w := badgeWidth(h, label)
	b := walk.Rectangle{X: right - w, Y: top, Width: w, Height: h}
	if err := icons.Fill(c, b, fill); err != nil {
		return err
	}
	return drawText(c, dpi, label, pt, walk.FontBold, text, b, centerText)
}

// drawText draws non-empty text in Segoe UI at pt points for a widget at dpi (DrawTextEx reports an
// empty string as a failure); text with emoji goes through drawColorText. walk sizes fonts by the canvas DPI, and its buffered canvas is a memory
// DC that reports the system DPI, not the window's: pt is rescaled so text matches the layout, which
// uses the window DPI. walk caches each resulting size.
func drawText(c *walk.Canvas, dpi int, text string, pt int, style walk.FontStyle, color walk.Color, r walk.Rectangle, format walk.DrawTextFormat) error {
	if text == "" || r.Width <= 0 || r.Height <= 0 {
		return nil
	}
	if hasEmoji(text) { // GDI draws emoji as outlines: DirectWrite draws them in colour
		cdpi := c.DPI()
		err := drawColorText(c.HDC(), text, float32(max(1, (pt*dpi+cdpi/2)/cdpi)*cdpi)/72, style, color, r, format)
		if err == nil {
			return nil
		}
		if !colourFailed { // once: every emoji drawn would say the same
			colourFailed = true
			logErr("colour emoji, drawn as outlines instead", err)
		}
	}
	return c.DrawTextPixels(text, textFont(pt, style, dpi, c.DPI()), color, r, format)
}

var colourFailed bool // drawColorText failed once and was reported

// textFont is the font drawText uses for pt points in a widget at dpi on a canvas that reports cdpi.
func textFont(pt int, style walk.FontStyle, dpi, cdpi int) *walk.Font {
	return font(max(1, (pt*dpi+cdpi/2)/cdpi), style)
}

// badgeWidth estimates the pill width from the digit count: measuring text on walk's buffered
// canvas leaks a metafile per paint.
func badgeWidth(h int, label string) int { return h + (len(label)-1)*h*7/20 }

// tipOnHover keeps cw's tooltip on label(x, y), the name of the item under the mouse ("" for none).
func tipOnHover(cw *walk.CustomWidget, label func(x, y int) string) {
	last := ""
	cw.MouseMove().Attach(func(x, y int, _ walk.MouseButton) {
		if l := label(x, y); l != last {
			last = l
			logErr("tooltip", cw.SetToolTipText(l))
		}
	})
}

// logErr reports an error from an event handler, which has no caller to return it to.
func logErr(op string, err error) {
	if err != nil {
		log.Printf("ui: %s: %v", op, err)
	}
}
