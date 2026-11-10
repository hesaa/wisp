// Package ui holds the main window's custom-painted widgets and the plain models behind them.
//
// Everything here is UI-thread only: whatsmeow events arrive on background goroutines and must be
// marshalled onto the UI thread (walk's Synchronize) before they reach a model or widget.
package ui

import (
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
	onAdd func()
	onNav func(Nav)
}

// NewRail creates the rail in parent.
func NewRail(parent walk.Container, p theme.Palette) (*Rail, error) {
	r := &Rail{model: NewRailModel(), pal: p}
	cw, err := newCanvas(parent, r.paint)
	if err != nil {
		return nil, err
	}
	r.CustomWidget = cw
	if err := cw.SetMinMaxSize(walk.Size{Width: railWidth}, walk.Size{Width: railWidth}); err != nil {
		cw.Dispose()
		return nil, err
	}
	r.model.redraw = func() { logErr("rail repaint", cw.Invalidate()) }
	cw.MouseDown().Attach(r.click)
	tipOnHover(cw, func(x, y int) string {
		it, ok := r.hit(x, y)
		switch {
		case !ok:
			return ""
		case it.kind == railAccount:
			return "+" + it.account
		case it.kind == railAdd:
			return "Add account"
		}
		return navNames[it.nav]
	})
	return r, nil
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

func (r *Rail) hit(x, y int) (railItem, bool) {
	for _, it := range r.items() {
		if it.kind != railDivider && y >= it.r.Y && y < it.r.Y+it.r.Height && x >= 0 && x < it.r.Width {
			return it, true
		}
	}
	return railItem{}, false
}

func (r *Rail) click(x, y int, button walk.MouseButton) {
	it, ok := r.hit(x, y)
	if !ok || button != walk.LeftButton {
		return
	}
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
	s := scaler(r.DPI())
	bg, err := brushFor(r.pal.Rail)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, r.ClientBoundsPixels()); err != nil {
		return err
	}
	accent := theme.Color(r.pal.Accent)
	for _, it := range r.items() {
		cx, cy := it.r.X+it.r.Width/2, it.r.Y+it.r.Height/2
		var err error
		switch it.kind {
		case railAccount:
			err = r.paintAccount(c, it.account, square(cx, cy, s(36)), s)
		case railAdd:
			if err = icons.Fill(c, square(cx, cy, s(36)), theme.Color(railFg.Header)); err == nil {
				err = icons.Draw(c, icons.IconAdd, square(cx, cy, s(20)), theme.Color(railFg.TextSecondary))
			}
		case railDivider:
			var line *walk.SolidColorBrush
			if line, err = brushFor(railFg.Header); err == nil {
				err = c.FillRectanglePixels(line, walk.Rectangle{X: s(14), Y: cy, Width: it.r.Width - s(28), Height: max(1, s(1))})
			}
		case railNav:
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

func (r *Rail) paintAccount(c *walk.Canvas, id string, av walk.Rectangle, s func(int) int) error {
	if err := drawAvatar(c, av, "", id, 10); err != nil {
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
	return drawBadge(c, av.X+av.Width+s(6), av.Y-s(4), s(18), label, theme.Color(r.pal.Accent), theme.Color(r.pal.Sidebar), 7)
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

func drawAvatar(c *walk.Canvas, r walk.Rectangle, name, jid string, pt int) error {
	if err := icons.Fill(c, r, avatarColor(jid)); err != nil {
		return err
	}
	return c.DrawTextPixels(initials(name, jid), font(pt, 0), walk.RGB(255, 255, 255), r, centerText)
}

// drawBadge paints an accent pill of height h holding label, right-aligned at right.
func drawBadge(c *walk.Canvas, right, top, h int, label string, fill, text walk.Color, pt int) error {
	w := badgeWidth(h, label)
	b := walk.Rectangle{X: right - w, Y: top, Width: w, Height: h}
	if err := icons.Fill(c, b, fill); err != nil {
		return err
	}
	return c.DrawTextPixels(label, font(pt, walk.FontBold), text, b, centerText)
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
