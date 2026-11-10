package ui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

const statusBroadcast = "status@broadcast" // stories: never a chat row

// SidebarModel is the active account's chat list, newest first, plus the search filter and the
// selected chat. It is the only chat list kept in memory. UI-thread only.
type SidebarModel struct {
	account  string
	chats    []store.Chat
	query    string       // lower-cased filter
	rows     []store.Chat // chats matching query; the chats slice itself when there is no filter
	selected string
	redraw   func(top bool) // set by the Sidebar; top: scroll back to the first row
}

func NewSidebarModel() *SidebarModel { return &SidebarModel{} }

// SetChats shows accountID's chats (newest first, as store.ListChats returns them) and takes
// ownership of the slice. Switching account clears the selection and scrolls to the top; reloading
// the same account (after a history sync) keeps both.
func (m *SidebarModel) SetChats(accountID string, chats []store.Chat) {
	switched := accountID != m.account
	m.account = accountID
	if switched {
		m.selected = ""
	}
	m.chats = slices.DeleteFunc(chats, func(c store.Chat) bool { return c.JID == statusBroadcast })
	m.changed(switched)
}

func (m *SidebarModel) Account() string { return m.account }

// Chats returns all of the account's chats, unfiltered. Do not modify.
func (m *SidebarModel) Chats() []store.Chat { return m.chats }

// Rows returns the chats matching the filter, newest first. Do not modify.
func (m *SidebarModel) Rows() []store.Chat { return m.rows }

func (m *SidebarModel) Selected() string { return m.selected }

// SetFilter keeps only chats whose name (or number, for unnamed chats) or last message contains q,
// ignoring case.
func (m *SidebarModel) SetFilter(q string) {
	m.query = strings.ToLower(strings.TrimSpace(q))
	m.changed(true)
}

// ApplyMessage folds a just-stored message (a whatsapp.MessageEvent) into the list: the chat takes
// the message as snippet and time, counts it as unread when it is incoming and unread, and moves up
// to its place by time (the top, for a live message). Messages of other accounts and stories are
// ignored. It reports whether the list changed.
func (m *SidebarModel) ApplyMessage(msg store.Message) bool {
	if msg.AccountID != m.account || msg.ChatJID == statusBroadcast {
		return false
	}
	c := store.Chat{JID: msg.ChatJID}
	if i := slices.IndexFunc(m.chats, func(o store.Chat) bool { return o.JID == msg.ChatJID }); i >= 0 {
		c = m.chats[i]
		m.chats = slices.Delete(m.chats, i, i+1)
	}
	if msg.Timestamp >= c.LastMessageTS { // a late older message doesn't replace the snippet
		c.LastMessageTS, c.Snippet = msg.Timestamp, msg.Body
	}
	if !msg.FromMe && msg.Status != store.StatusRead {
		c.Unread++
	}
	at := slices.IndexFunc(m.chats, func(o store.Chat) bool { return o.LastMessageTS <= c.LastMessageTS })
	if at < 0 {
		at = len(m.chats)
	}
	m.chats = slices.Insert(m.chats, at, c)
	m.changed(false)
	return true
}

// MarkRead clears jid's unread badge (after the chat was read).
func (m *SidebarModel) MarkRead(jid string) {
	if i := slices.IndexFunc(m.chats, func(c store.Chat) bool { return c.JID == jid }); i >= 0 && m.chats[i].Unread != 0 {
		m.chats[i].Unread = 0
		m.changed(false)
	}
}

// Select highlights jid ("" for none).
func (m *SidebarModel) Select(jid string) {
	m.selected = jid
	if m.redraw != nil {
		m.redraw(false)
	}
}

// Unread is the account's unread total, for the rail badge.
func (m *SidebarModel) Unread() int {
	n := 0
	for _, c := range m.chats {
		n += c.Unread
	}
	return n
}

func (m *SidebarModel) changed(top bool) {
	m.rows = m.chats
	if m.query != "" {
		m.rows = nil
		for _, c := range m.chats {
			if strings.Contains(strings.ToLower(displayName(c)), m.query) || strings.Contains(strings.ToLower(c.Snippet), m.query) {
				m.rows = append(m.rows, c)
			}
		}
	}
	if m.redraw != nil {
		m.redraw(top)
	}
}

// displayName is the chat's name, else its phone number.
func displayName(c store.Chat) string {
	if c.Name != "" {
		return c.Name
	}
	user, server, _ := strings.Cut(c.JID, "@")
	if server == "s.whatsapp.net" {
		return "+" + user
	}
	return user
}

// relTime formats a chat time like WhatsApp: clock time today, then Yesterday, the weekday within a
// week, else the date.
func relTime(ts int64, now time.Time) string {
	if ts <= 0 {
		return ""
	}
	t := time.Unix(ts, 0).In(now.Location())
	y, mo, d := now.Date()
	today := time.Date(y, mo, d, 0, 0, 0, 0, now.Location())
	switch {
	case !t.Before(today):
		return t.Format("15:04")
	case !t.Before(today.AddDate(0, 0, -1)):
		return "Yesterday"
	case !t.Before(today.AddDate(0, 0, -6)):
		return t.Weekday().String()
	}
	return t.Format("02/01/2006")
}

// Sidebar is the 280px left column: stories strip, "Chats" header with the New Chat button, search
// box and the chat list. Feed it through Model() and SetStories. Tab moves between its parts; Down in
// the search box enters the list and Enter there opens the first visible row. UI-thread only.
type Sidebar struct {
	*walk.Composite
	model     *SidebarModel
	pal       theme.Palette
	stories   *storyStrip
	header    *walk.CustomWidget
	search    *searchBox
	list      *chatList
	onNewChat func()
	onChat    func(jid string)
}

// NewSidebar creates the sidebar in parent.
func NewSidebar(parent walk.Container, p theme.Palette) (*Sidebar, error) {
	comp, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	s := &Sidebar{Composite: comp, model: NewSidebarModel(), pal: p}
	if err := s.build(); err != nil {
		comp.Dispose()
		return nil, err
	}
	return s, nil
}

func (s *Sidebar) build() error {
	var err error
	if err = errors.Join(setBox(s.Composite, walk.NewVBoxLayout(), walk.Margins{}),
		s.SetMinMaxSize(walk.Size{Width: sidebarWidth}, walk.Size{Width: sidebarWidth})); err != nil {
		return err
	}
	if s.stories, err = newStoryStrip(s.Composite, &s.pal); err != nil {
		return err
	}
	if s.header, err = s.newHeader(); err != nil {
		return err
	}
	if s.search, err = newSearchBox(s.Composite, &s.pal, "Search or start new chat"); err != nil {
		return err
	}
	if s.list, err = newChatList(s.Composite, &s.pal, "Chats"); err != nil {
		return err
	}
	if err := fillRest(s.Composite, s.list); err != nil {
		return err
	}
	s.stories.SetVisible(false) // until SetStories has some
	s.search.edit.TextChanged().Attach(func() { s.model.SetFilter(s.search.edit.Text()) })
	searchKeys(s.search.edit, s.list)
	s.list.onClick = func(jid string) {
		s.model.Select(jid)
		if s.onChat != nil {
			s.onChat(jid)
		}
	}
	s.model.redraw = func(top bool) { s.list.setRows(s.model.Rows(), s.model.Selected(), top) }
	return s.SetPalette(s.pal)
}

func (s *Sidebar) Model() *SidebarModel { return s.model }

// SetStories shows the stories strip (contacts with status updates, newest first); it is hidden
// when there are none.
func (s *Sidebar) SetStories(st []store.Story) {
	s.stories.stories = st
	s.stories.offset, s.stories.focus = 0, -1
	s.stories.SetVisible(len(st) > 0)
	logErr("stories repaint", s.stories.Invalidate())
}

// FocusSearch puts the keyboard focus in the search box.
func (s *Sidebar) FocusSearch() error { return s.search.edit.SetFocus() }

// OnChatSelected sets the handler of a click on a chat row (the row is selected first).
func (s *Sidebar) OnChatSelected(fn func(jid string)) { s.onChat = fn }

// OnStorySelected sets the handler of a click on a story avatar.
func (s *Sidebar) OnStorySelected(fn func(senderJID string)) { s.stories.onClick = fn }

// OnNewChat sets the handler of the New Chat button; it typically runs RunNewChat.
func (s *Sidebar) OnNewChat(fn func()) { s.onNewChat = fn }

// SetPalette recolours the sidebar.
func (s *Sidebar) SetPalette(p theme.Palette) error {
	s.pal = p
	return errors.Join(setBg(s.Composite, p.Sidebar), s.search.applyPalette(), s.stories.Invalidate(), s.header.Invalidate(), s.list.Invalidate())
}

// newHeader is the "Chats" title with the New Chat button, which is a tab stop (Enter or Space).
func (s *Sidebar) newHeader() (*walk.CustomWidget, error) {
	var h *walk.CustomWidget
	onButton := func(x int) bool { return x >= h.ClientBoundsPixels().Width-h.IntFrom96DPI(52) }
	newChat := func() {
		if s.onNewChat != nil {
			s.onNewChat()
		}
	}
	h, err := newCanvas(s.Composite, func(c *walk.Canvas, _ walk.Rectangle) error {
		dpi, b, p := h.DPI(), h.ClientBoundsPixels(), s.pal
		sc := scaler(dpi)
		bg, err := brushFor(p.Sidebar)
		if err != nil {
			return err
		}
		cx, cy := b.Width-sc(30), b.Height/2
		if err := c.FillRectanglePixels(bg, b); err != nil {
			return err
		}
		if focusCues(h) {
			if err := focusRing(c, cx, cy, sc(40), sc(2), theme.Color(p.Text), theme.Color(p.Sidebar)); err != nil {
				return err
			}
		}
		return errors.Join(
			drawText(c, dpi, "Chats", 15, walk.FontBold, theme.Color(p.Text), walk.Rectangle{X: sc(16), Width: b.Width - sc(70), Height: b.Height}, leftText),
			icons.Draw(c, icons.IconNewChat, square(cx, cy, sc(24)), theme.Color(p.TextSecondary)))
	})
	if err != nil {
		return nil, err
	}
	if err := errors.Join(h.SetMinMaxSize(walk.Size{Height: 52}, walk.Size{Height: 52}),
		focusable(h, walk.AccRolePushbutton, "New chat", func(k walk.Key) bool {
			if k == walk.KeyReturn || k == walk.KeySpace {
				newChat()
				return true
			}
			return false
		})); err != nil {
		return nil, err
	}
	h.MouseDown().Attach(func(x, _ int, b walk.MouseButton) {
		if b != walk.LeftButton {
			return
		}
		logErr("focus", h.SetFocus())
		if onButton(x) {
			newChat()
		}
	})
	tipOnHover(h, func(x, _ int) string {
		if onButton(x) {
			return "New chat"
		}
		return ""
	})
	return h, nil
}

// storyStrip is the horizontally scrolling row of contacts with recent status updates.
type storyStrip struct {
	*walk.CustomWidget
	pal     *theme.Palette
	stories []store.Story
	offset  int // horizontal scroll, pixels
	focus   int // keyboard focus, -1 for none
	onClick func(senderJID string)
}

const (
	storyW    = 68 // one story slot, 1/96 inch
	storyName = "Status updates"
)

// newStoryStrip creates the strip; it is a tab stop: Left/Right move the focus, Enter or Space open.
func newStoryStrip(parent walk.Container, pal *theme.Palette) (*storyStrip, error) {
	st := &storyStrip{pal: pal, focus: -1}
	cw, err := newCanvas(parent, st.paint)
	if err != nil {
		return nil, err
	}
	st.CustomWidget = cw
	if err := errors.Join(cw.SetMinMaxSize(walk.Size{Height: 92}, walk.Size{Height: 92}),
		focusable(cw, walk.AccRoleList, storyName, st.key)); err != nil {
		return nil, err
	}
	cw.MouseDown().Attach(func(x, _ int, b walk.MouseButton) {
		if b != walk.LeftButton {
			return
		}
		// resolve the story against the offset the user saw, then take focus, then open it
		i := st.span().at(x)
		if i >= 0 {
			st.setFocus(i)
		}
		logErr("focus", cw.SetFocus())
		if i >= 0 {
			st.open(i)
		}
	})
	cw.FocusedChanged().Attach(func() {
		if i := st.span().firstFull(); cw.Focused() && st.focus < 0 && i >= 0 {
			st.setFocus(i) // no scrolling: a click may be resolving its story against this offset
		}
	})
	cw.MouseWheel().Attach(func(_, _ int, b walk.MouseButton) {
		if off := st.clamp(st.offset - walk.MouseWheelEventDelta(b)*cw.IntFrom96DPI(storyW)/120); off != st.offset {
			st.offset = off
			logErr("stories repaint", cw.Invalidate())
		}
	})
	return st, nil
}

func (st *storyStrip) clamp(off int) int {
	s := scaler(st.DPI())
	return max(0, min(off, s(16)+len(st.stories)*s(storyW)-st.ClientBoundsPixels().Width))
}

func (st *storyStrip) open(i int) {
	if st.onClick != nil {
		st.onClick(st.stories[i].SenderJID)
	}
}

func (st *storyStrip) span() span {
	return span{off: st.offset, pad: st.IntFrom96DPI(8), size: st.IntFrom96DPI(storyW), n: len(st.stories)}
}

// setFocus moves the focus to story i, without scrolling.
func (st *storyStrip) setFocus(i int) {
	st.focus = i
	logErr("stories repaint", st.Invalidate())
	story := st.stories[i]
	announce(st, storyName, displayName(store.Chat{JID: story.SenderJID, Name: story.Name}))
}

// focusOn moves the focus to story i and scrolls it into view.
func (st *storyStrip) focusOn(i int) {
	st.setFocus(i)
	g := st.span()
	st.offset = st.clamp(scrollIntoView(st.offset, g.pad+i*g.size, g.size, st.ClientBoundsPixels().Width))
}

func (st *storyStrip) key(k walk.Key) bool {
	if k == walk.KeyReturn || k == walk.KeySpace {
		if st.focus >= 0 && st.focus < len(st.stories) {
			st.open(st.focus)
		}
		return true
	}
	next, ok := navigate(st.focus, len(st.stories), st.ClientBoundsPixels().Width/st.IntFrom96DPI(storyW), k, false)
	if ok && next >= 0 {
		st.focusOn(next)
	}
	return ok
}

func (st *storyStrip) paint(c *walk.Canvas, ub walk.Rectangle) error {
	dpi, p, b := st.DPI(), st.pal, st.ClientBoundsPixels()
	s := scaler(dpi)
	focused := focusCues(st)
	st.offset = st.clamp(st.offset)
	bg, err := brushFor(p.Sidebar)
	if err != nil {
		return err
	}
	line, err := brushFor(p.Header)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, ub); err != nil {
		return err
	}
	for i, story := range st.stories {
		x := s(8) + i*s(storyW) - st.offset
		if x+s(storyW) <= ub.X || x >= ub.X+ub.Width {
			continue
		}
		cx, cy := x+s(storyW)/2, s(34)
		name := displayName(store.Chat{JID: story.SenderJID, Name: story.Name})
		if f := strings.Fields(story.Name); len(f) > 0 {
			name = f[0]
		}
		if focused && i == st.focus {
			if err := focusRing(c, cx, cy, s(60), s(2), theme.Color(p.Text), theme.Color(p.Sidebar)); err != nil {
				return err
			}
		}
		if err := errors.Join(
			icons.Fill(c, square(cx, cy, s(52)), theme.Color(p.Accent)), // unseen-update ring
			icons.Fill(c, square(cx, cy, s(48)), theme.Color(p.Sidebar)),
			drawAvatar(c, dpi, square(cx, cy, s(44)), story.Name, story.SenderJID, 12),
			drawText(c, dpi, name, 8, 0, theme.Color(p.Text), walk.Rectangle{X: x + s(2), Y: s(64), Width: s(storyW - 4), Height: s(18)}, centerText),
		); err != nil {
			return err
		}
	}
	return c.FillRectanglePixels(line, walk.Rectangle{Y: b.Height - max(1, s(1)), Width: b.Width, Height: max(1, s(1))})
}

// searchBox is a borderless LineEdit with a search icon on a rounded pill.
type searchBox struct {
	*walk.Composite // padding row in the sidebar colour
	pill            *walk.Composite
	icon            *walk.CustomWidget
	edit            *walk.LineEdit
	pal             *theme.Palette
}

func newSearchBox(parent walk.Container, pal *theme.Palette, cue string) (*searchBox, error) {
	row, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	b := &searchBox{Composite: row, pal: pal}
	if err := b.build(cue); err != nil {
		row.Dispose()
		return nil, err
	}
	return b, nil
}

func (b *searchBox) build(cue string) error {
	var err error
	if b.pill, err = walk.NewComposite(b.Composite); err != nil {
		return err
	}
	pl := walk.NewHBoxLayout()
	if err := errors.Join(setBox(b.Composite, walk.NewVBoxLayout(), walk.Margins{HNear: 12, VNear: 4, HFar: 12, VFar: 8}),
		setBox(b.pill, pl, walk.Margins{HNear: 4, HFar: 10}), pl.SetAlignment(walk.AlignHNearVCenter),
		b.pill.SetMinMaxSize(walk.Size{Height: 36}, walk.Size{Height: 36})); err != nil {
		return err
	}
	b.icon, err = newCanvas(b.pill, func(c *walk.Canvas, _ walk.Rectangle) error {
		r := b.icon.ClientBoundsPixels()
		bg, err := brushFor(b.pal.Header)
		if err != nil {
			return err
		}
		return errors.Join(c.FillRectanglePixels(bg, r),
			icons.Draw(c, icons.IconSearch, square(r.Width/2, r.Height/2, b.icon.IntFrom96DPI(18)), theme.Color(b.pal.TextSecondary)))
	})
	if err != nil {
		return err
	}
	if b.edit, err = walk.NewLineEdit(b.pill); err != nil {
		return err
	}
	b.edit.SetFont(font(10, 0))
	return errors.Join(b.icon.SetMinMaxSize(walk.Size{Width: 34}, walk.Size{Width: 34}),
		b.SetMinMaxSize(walk.Size{Height: 48}, walk.Size{Height: 48}), cueBanner(b.edit, cue), noBorder(b.edit))
}

// cueBanner sets the grey hint, kept while the box is focused and empty (walk's SetCueBanner hides it
// on focus).
func cueBanner(le *walk.LineEdit, text string) error {
	p, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return err
	}
	if le.SendMessage(win.EM_SETCUEBANNER, win.TRUE, uintptr(unsafe.Pointer(p))) == win.FALSE {
		return fmt.Errorf("ui: EM_SETCUEBANNER failed")
	}
	return nil
}

func (b *searchBox) applyPalette() error {
	b.edit.SetTextColor(theme.Color(b.pal.Text))
	return errors.Join(setBg(b.Composite, b.pal.Sidebar), setBg(b.pill, b.pal.Header), setBg(b.edit, b.pal.Header), b.icon.Invalidate())
}

// noBorder drops the sunken client edge walk gives every LineEdit and TextEdit.
func noBorder(w walk.Window) error {
	h := w.Handle()
	ex := win.GetWindowLong(h, win.GWL_EXSTYLE)
	if win.SetWindowLong(h, win.GWL_EXSTYLE, ex&^win.WS_EX_CLIENTEDGE) == 0 {
		return fmt.Errorf("ui: SetWindowLong(GWL_EXSTYLE) failed")
	}
	if !win.SetWindowPos(h, 0, 0, 0, 0, 0, win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_FRAMECHANGED) {
		return fmt.Errorf("ui: SetWindowPos(SWP_FRAMECHANGED) failed")
	}
	return nil
}

// chatList is a custom-painted, scrollable list of chats that paints only the visible rows: avatar,
// name, time, one-line snippet and unread badge. Wheel and a draggable thumb scroll it. It is a tab
// stop: Up/Down/PgUp/PgDn/Home/End move the focused row, Enter or Space open it like a click.
type chatList struct {
	*walk.CustomWidget
	vscroll
	pal      *theme.Palette
	name     string // accessible name
	rows     []store.Chat
	selected string
	focus    string // JID of the keyboard-focused row, so it survives reordering
	onClick  func(jid string)
}

func newChatList(parent walk.Container, pal *theme.Palette, name string) (*chatList, error) {
	l := &chatList{pal: pal, name: name}
	cw, err := newCanvas(parent, l.paint)
	if err != nil {
		return nil, err
	}
	l.CustomWidget = cw
	if err := focusable(cw, walk.AccRoleList, name, l.key); err != nil {
		return nil, err
	}
	cw.FocusedChanged().Attach(func() {
		if i := l.span().firstFull(); cw.Focused() && l.focusIndex() < 0 && i >= 0 {
			l.setFocus(i) // no scrolling: a click may be resolving its row against this offset
		}
	})
	cw.MouseDown().Attach(l.mouseDown)
	l.attach(cw, func() int { return len(l.rows) * l.rowH() }, func() int { return l.rowH() * 3 / 2 })
	return l, nil
}

func (l *chatList) rowH() int { return l.IntFrom96DPI(72) }

func (l *chatList) focusIndex() int {
	if l.focus == "" {
		return -1
	}
	return slices.IndexFunc(l.rows, func(c store.Chat) bool { return c.JID == l.focus })
}

func (l *chatList) span() span { return span{off: l.off, size: l.rowH(), n: len(l.rows)} }

// setFocus moves the keyboard focus to row i, without scrolling.
func (l *chatList) setFocus(i int) {
	row := l.rows[i]
	l.focus = row.JID
	logErr("chat list repaint", l.Invalidate())
	item := displayName(row)
	if row.Unread > 0 {
		item += ", " + strconv.Itoa(row.Unread) + " unread"
	}
	announce(l, l.name, item)
}

// focusRow moves the keyboard focus to row i and scrolls it into view.
func (l *chatList) focusRow(i int) {
	l.setFocus(i)
	rowH := l.rowH()
	l.scrollTo(scrollIntoView(l.off, i*rowH, rowH, l.ClientBoundsPixels().Height))
}

func (l *chatList) open(i int) {
	l.focus = l.rows[i].JID
	if l.onClick != nil {
		l.onClick(l.rows[i].JID)
	}
}

// openFirstVisible opens the first wholly visible row (Enter in the search box).
func (l *chatList) openFirstVisible() {
	if i := l.span().firstFull(); i >= 0 {
		l.open(i)
	}
}

func (l *chatList) key(k walk.Key) bool {
	i := l.focusIndex()
	if k == walk.KeyReturn || k == walk.KeySpace {
		if i >= 0 {
			l.open(i)
		}
		return true
	}
	next, ok := navigate(i, len(l.rows), l.ClientBoundsPixels().Height/l.rowH(), k, false)
	if ok && next >= 0 {
		l.focusRow(next)
	}
	return ok
}

// searchKeys links a search box to its list: Down moves into the list, Enter opens the first visible row.
func searchKeys(edit *walk.LineEdit, l *chatList) {
	edit.KeyDown().Attach(func(k walk.Key) {
		switch {
		case k == walk.KeyDown && len(l.rows) > 0:
			logErr("focus", l.SetFocus())
			showFocusCues(l.Handle())
		case k == walk.KeyReturn:
			l.openFirstVisible()
		}
	})
}

// setRows shows rows (top: from the first row) with selected highlighted.
func (l *chatList) setRows(rows []store.Chat, selected string, top bool) {
	l.rows, l.selected = rows, selected
	if top {
		l.off = 0
	}
	l.off = l.clamp(l.off)
	logErr("chat list repaint", l.Invalidate())
}

func (l *chatList) mouseDown(x, y int, b walk.MouseButton) {
	if b != walk.LeftButton {
		return
	}
	if l.press(x, y) {
		return
	}
	// resolve the row against the offset the user saw, then take focus, then open it
	i := l.span().at(y)
	if i >= 0 {
		l.setFocus(i)
	}
	logErr("focus", l.SetFocus())
	if i >= 0 {
		l.open(i)
	}
}

func (l *chatList) paint(c *walk.Canvas, ub walk.Rectangle) error {
	p, b, rowH := l.pal, l.ClientBoundsPixels(), l.rowH()
	l.off = l.clamp(l.off)
	bg, err := brushFor(p.Sidebar)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, ub); err != nil {
		return err
	}
	now, focused := time.Now(), focusCues(l)
	last := min(len(l.rows), (ub.Y+ub.Height+l.off+rowH-1)/rowH)
	for i := max(0, (ub.Y+l.off)/rowH); i < last; i++ {
		r := walk.Rectangle{Y: i*rowH - l.off, Width: b.Width, Height: rowH}
		if err := l.paintRow(c, l.rows[i], r, now); err != nil {
			return err
		}
		if focused && l.rows[i].JID == l.focus {
			if err := focusRect(c, r, max(1, l.IntFrom96DPI(2)), p.Text); err != nil {
				return err
			}
		}
	}
	if t := l.thumb(); t.Height > 0 {
		return icons.Fill(c, t, theme.Color(p.TextSecondary))
	}
	return nil
}

func (l *chatList) paintRow(c *walk.Canvas, row store.Chat, r walk.Rectangle, now time.Time) error {
	dpi, p := l.DPI(), l.pal
	s := scaler(dpi)
	if row.JID == l.selected {
		sel, err := brushFor(p.Header)
		if err != nil {
			return err
		}
		if err := c.FillRectanglePixels(sel, r); err != nil {
			return err
		}
	}
	line, err := brushFor(p.Header)
	if err != nil {
		return err
	}
	av := walk.Rectangle{X: r.X + s(12), Y: r.Y + (r.Height-s(48))/2, Width: s(48), Height: s(48)}
	x, right := av.X+av.Width+s(13), r.X+r.Width-s(14)
	ts := relTime(row.LastMessageTS, now)
	tw := len(ts) * s(7) // width estimate: measuring text on walk's buffered canvas leaks a metafile
	timeColor, snipRight := theme.Color(p.TextSecondary), right
	if row.Unread > 0 {
		timeColor = theme.Color(p.Accent)
		label := strconv.Itoa(row.Unread)
		if row.Unread > 99 {
			label = "99+"
		}
		if err := drawBadge(c, dpi, right, r.Y+s(40), s(20), label, theme.Color(p.Accent), theme.Color(p.Sidebar), 8); err != nil {
			return err
		}
		snipRight -= badgeWidth(s(20), label) + s(6)
	}
	return errors.Join(
		drawAvatar(c, dpi, av, row.Name, row.JID, 13),
		drawText(c, dpi, ts, 9, 0, timeColor, walk.Rectangle{X: right - tw, Y: r.Y + s(13), Width: tw, Height: s(24)}, rightText),
		drawText(c, dpi, displayName(row), 12, 0, theme.Color(p.Text), walk.Rectangle{X: x, Y: r.Y + s(13), Width: right - tw - s(6) - x, Height: s(24)}, leftText),
		drawText(c, dpi, oneLine(row.Snippet), 10, 0, theme.Color(p.TextSecondary), walk.Rectangle{X: x, Y: r.Y + s(38), Width: snipRight - x, Height: s(22)}, leftText),
		c.FillRectanglePixels(line, walk.Rectangle{X: x, Y: r.Y + r.Height - max(1, s(1)), Width: r.Width - x, Height: max(1, s(1))}),
	)
}

// oneLine is a snippet's first 200 bytes (on a rune boundary) with line breaks as spaces.
func oneLine(s string) string {
	if len(s) > 200 {
		i := 200
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		s = s[:i]
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", " ")
}

// setBox installs layout l on c with margins m (1/96 inch) and no spacing.
func setBox(c walk.Container, l *walk.BoxLayout, m walk.Margins) error {
	return errors.Join(l.SetMargins(m), l.SetSpacing(0), c.SetLayout(l))
}

// fillRest gives w nearly all of c's spare length. walk's box layout shares spare space among greedy
// items (every custom widget is one) by stretch factor, and space offered to a size-capped item is
// left as a gap rather than passed on.
func fillRest(c walk.Container, w walk.Widget) error {
	return c.Layout().(*walk.BoxLayout).SetStretchFactor(w, 1000)
}
