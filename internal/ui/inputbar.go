package ui

import (
	"cmp"
	"errors"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

// Sizes in 1/96 inch.
const (
	barButton     = 40 // the bar's round buttons
	barPadY       = 8  // above and below the box and buttons
	fieldPadY     = 10 // between the box's background and its text
	inputMaxLines = 5  // the box grows to this many lines, then scrolls
	replyBannerH  = 42 // the "Replying to" banner over the box, with its gap
	replyCloseW   = 28 // the banner's × at its right
	mentionRowH   = 32 // a row of the mention list over the box
	mentionRows   = 5  // rows the list shows at most; it scrolls with the highlight
)

// typingIdle is how long after the last edit typing counts as paused.
const typingIdle = 5 * time.Second

// InputBar is the composer under the chat: Emoji, GIF and Attach buttons, a multi-line message box
// that grows to five lines and then scrolls, and the Send button, disabled while the box is blank.
// Enter sends and Shift+Enter breaks the line. Every button is a tab stop activated by Enter or
// Space; the Emoji and GIF popups close on Esc (focus returns to their button), a pick, or a click
// elsewhere in the window. A pending reply shows in a banner over the box (SetReply); in a group,
// typing @ lists the members to mention over it: Up and Down move, Enter or Tab pick, Esc closes.
// UI-thread only.
type InputBar struct {
	*walk.Composite
	composer
	pal                         theme.Palette
	emoji, gif, attach, sendBtn *walk.CustomWidget
	field                       *walk.Composite // the box's background
	edit                        *walk.TextEdit
	banner, list                *walk.CustomWidget // inside field over the box: pending reply, mention list
	height                      int                // the bar's height, 1/96 inch
	listH                       int                // the mention list's height, 1/96 inch; 0: hidden
	canSend                     bool               // the box holds more than whitespace
	gifs                        *giphy
	gifKey                      func() string
	popup                       *walk.Dialog                                       // the open popup, nil for none
	after                       func(d time.Duration, f func()) (stop func() bool) // one-shot timer; f runs on the UI thread
	onSendGIF                   func(mp4URL string)
	onAttach                    func(path string)
}

// NewInputBar creates the bar in parent. gifKey returns the user's GIPHY API key ("" when none is
// set); GIF search calls it, on the UI thread, each time it searches.
func NewInputBar(parent walk.Container, p theme.Palette, gifKey func() string) (*InputBar, error) {
	comp, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	b := &InputBar{Composite: comp, pal: p, gifs: newGiphy(), gifKey: gifKey}
	b.after = func(d time.Duration, f func()) func() bool {
		return time.AfterFunc(d, func() { b.Synchronize(f) }).Stop
	}
	b.typing.after = b.after
	if err := b.build(); err != nil {
		comp.Dispose()
		return nil, err
	}
	return b, nil
}

func (b *InputBar) build() error {
	l := walk.NewHBoxLayout()
	if err := errors.Join(setBox(b.Composite, l, walk.Margins{HNear: 8, VNear: barPadY, HFar: 12, VFar: barPadY}),
		l.SetSpacing(4), l.SetAlignment(walk.AlignHNearVFar)); err != nil { // buttons stay at the bottom as the box grows
		return err
	}
	var err error
	if b.emoji, err = b.newButton("Emoji", icons.IconEmoji, b.pickEmoji); err != nil {
		return err
	}
	if b.gif, err = b.newButton("GIF", icons.IconGIF, b.pickGIF); err != nil {
		return err
	}
	if b.attach, err = b.newButton("Attach", icons.IconAttach, b.pickFile); err != nil {
		return err
	}
	if b.field, err = walk.NewComposite(b.Composite); err != nil {
		return err
	}
	if err := setBox(b.field, walk.NewVBoxLayout(), walk.Margins{HNear: 12, VNear: fieldPadY, HFar: 12, VFar: fieldPadY}); err != nil {
		return err
	}
	if b.list, err = newCanvas(b.field, b.paintMentions); err != nil {
		return err
	}
	if b.banner, err = newCanvas(b.field, b.paintBanner); err != nil {
		return err
	}
	b.list.SetVisible(false)
	b.banner.SetVisible(false)
	if err := b.banner.SetMinMaxSize(walk.Size{Height: replyBannerH}, walk.Size{Height: replyBannerH}); err != nil {
		return err
	}
	b.list.MouseDown().Attach(func(_, y int, btn walk.MouseButton) {
		if i := b.mentionTop() + y/b.list.IntFrom96DPI(mentionRowH); btn == walk.LeftButton && i < len(b.picker.items) {
			b.picker.sel = i
			b.completeMention()
		}
	})
	b.banner.MouseDown().Attach(func(x, _ int, btn walk.MouseButton) {
		if btn == walk.LeftButton && x >= b.banner.ClientBoundsPixels().Width-b.banner.IntFrom96DPI(replyCloseW) {
			b.ClearReply()
		}
		focusOn(b.edit)
	})
	// ES_AUTOVSCROLL: past the fifth line the box scrolls instead of refusing text
	if b.edit, err = walk.NewTextEditWithStyle(b.field, win.ES_AUTOVSCROLL); err != nil {
		return err
	}
	if b.sendBtn, err = b.newButton("Send", icons.IconSend, b.sendNow); err != nil {
		return err
	}
	b.box = b.edit
	b.edit.SetFont(font(bodyPt, 0))
	if err := errors.Join(noBorder(b.edit), fillRest(b.Composite, b.field), fillRest(b.field, b.edit), enterSendsIn(b.edit, b.sendNow, b.boxKey),
		b.edit.Accessibility().SetName("Type a message")); err != nil {
		return err
	}
	b.sendBtn.SetEnabled(false)
	b.edit.TextChanged().Attach(b.edited)
	b.edit.SizeChanged().Attach(b.fit) // rewrapped, or a new DPI
	b.fit()
	return b.SetPalette(b.pal)
}

// OnSend sets the handler of a sent message: the box's text, trimmed, never blank.
func (b *InputBar) OnSend(fn func(text string)) { b.onSend = fn }

// OnTyping sets the handler of typing state changes: true on the first edit after a pause, false
// typingIdle after the last edit, on send, and when the box is emptied or Reset.
func (b *InputBar) OnTyping(fn func(composing bool)) { b.typing.fire = fn }

// OnSendGIF sets the handler of a GIF picked in the search popup: an https GIPHY MP4 URL.
func (b *InputBar) OnSendGIF(fn func(mp4URL string)) { b.onSendGIF = fn }

// OnAttach sets the handler of a file picked with the Attach button.
func (b *InputBar) OnAttach(fn func(path string)) { b.onAttach = fn }

// Reset empties the box for another chat and drops its pending reply and mention list. Call it
// before switching: it may fire OnTyping(false) for the chat being left.
func (b *InputBar) Reset() {
	b.composer.reset()
	b.banner.SetVisible(false)
	b.showMentions()
	b.fit()
}

// DismissMentions closes the mention list, if open.
func (b *InputBar) DismissMentions() {
	b.picker.close()
	b.showMentions()
}

// SetReply makes the next message a reply to m, shown in a banner over the box ("Replying to
// author") until it is sent or cancelled (the banner's ×, or Esc in a blank box).
func (b *InputBar) SetReply(m store.Message, author string) {
	b.composer.setReply(m, author)
	b.banner.SetVisible(true)
	logErr("reply banner repaint", b.banner.Invalidate())
	b.fit()
}

// ClearReply drops the pending reply, if any.
func (b *InputBar) ClearReply() {
	if b.reply == nil {
		return
	}
	b.composer.clearReply()
	b.banner.SetVisible(false)
	b.fit()
}

// ReplyTarget is the message the next one replies to; ok is false for none.
func (b *InputBar) ReplyTarget() (store.Message, bool) { return b.composer.replyTarget() }

// SetGroupParticipants gives the members typing @ offers to mention: the open group's, none for
// any other chat. They are kept in memory; typing never looks anything up.
func (b *InputBar) SetGroupParticipants(members []GroupMember) {
	members = slices.Clone(members)
	slices.SortStableFunc(members, func(x, y GroupMember) int {
		return cmp.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name))
	})
	if slices.Equal(members, b.picker.members) {
		return // an open list keeps its highlight
	}
	b.picker.members = members
	b.refreshMentions()
}

// WireMentions turns the @Name mentions of a sent text into WhatsApp's @<number> form and returns
// it with the JIDs mentioned.
func (b *InputBar) WireMentions(text string) (string, []string) {
	return wireMentions(text, b.picker.members)
}

// caretSplit is the box's text before and after the caret.
func (b *InputBar) caretSplit() (before, after string) {
	u := utf16.Encode([]rune(b.edit.Text()))
	start, _ := b.edit.TextSelection()
	start = max(0, min(start, len(u)))
	return string(utf16.Decode(u[:start])), string(utf16.Decode(u[start:]))
}

// refreshMentions opens, updates or closes the mention list for the text before the caret.
func (b *InputBar) refreshMentions() {
	before, _ := b.caretSplit()
	b.picker.refresh(before)
	b.showMentions()
}

// showMentions sizes the list for its rows (none: hidden) and repaints it.
func (b *InputBar) showMentions() {
	h := min(len(b.picker.items), mentionRows) * mentionRowH
	if h != b.listH { // not on every keystroke
		b.listH = h
		b.list.SetVisible(h > 0)
		logErr("mention list", b.list.SetMinMaxSize(walk.Size{Height: h}, walk.Size{Height: h}))
		b.fit()
	}
	logErr("mention list repaint", b.list.Invalidate())
}

// completeMention puts the highlighted mention in the box and the caret after it.
func (b *InputBar) completeMention() {
	text, caret := b.picker.complete(b.caretSplit())
	logErr("mention", b.edit.SetText(text))
	b.edit.SetTextSelection(caret, caret)
	b.showMentions()
	focusOn(b.edit)
}

// boxKey is a key pressed in the box: the open mention list takes Up, Down, Enter, Tab and Esc, and
// Esc in a blank box cancels a pending reply. With act false it only reports whether it would be
// taken. Shift+Enter always breaks the line.
func (b *InputBar) boxKey(k walk.Key, act bool) bool {
	if len(b.picker.items) == 0 {
		if k != walk.KeyEscape || b.reply == nil {
			return false
		}
		if _, typed := outgoingText(b.edit.Text()); typed {
			return false
		}
		if act {
			b.ClearReply()
		}
		return true
	}
	switch {
	case k == walk.KeyUp || k == walk.KeyDown:
		if act {
			b.picker.move(int(k) - int(walk.KeyUp) - 1) // Up -1, Down +1
			logErr("mention list repaint", b.list.Invalidate())
		}
	case k == walk.KeyTab || k == walk.KeyReturn && !walk.ShiftDown():
		if act {
			b.completeMention()
		}
	case k == walk.KeyEscape:
		if act {
			b.picker.close()
			b.showMentions()
		}
	default:
		return false
	}
	return true
}

// mentionTop is the first row the list shows: it scrolls just enough to keep the highlight in.
func (b *InputBar) mentionTop() int { return max(0, b.picker.sel-mentionRows+1) }

func (b *InputBar) paintMentions(c *walk.Canvas, _ walk.Rectangle) error {
	p, r, dpi := b.pal, b.list.ClientBoundsPixels(), b.list.DPI()
	s := scaler(dpi)
	bg, err := brushFor(p.Sidebar)
	if err != nil {
		return err
	}
	hl, err := brushFor(p.Selection)
	if err != nil {
		return err
	}
	errs := []error{c.FillRectanglePixels(bg, r)}
	top := b.mentionTop()
	for i := top; i < len(b.picker.items) && i < top+mentionRows; i++ {
		m, row := b.picker.items[i], walk.Rectangle{Y: (i - top) * s(mentionRowH), Width: r.Width, Height: s(mentionRowH)}
		if i == b.picker.sel {
			errs = append(errs, c.FillRectanglePixels(hl, row))
		}
		av := walk.Rectangle{X: s(4), Y: row.Y + (row.Height-s(24))/2, Width: s(24), Height: s(24)}
		title, sub := "@"+m.Name, "Notify all participants"
		if m.JID == "" {
			errs = append(errs, icons.Fill(c, av, theme.Color(p.Accent)), drawText(c, dpi, "@", 10, walk.FontBold, walk.RGB(255, 255, 255), av, centerText))
		} else {
			errs = append(errs, drawAvatar(c, dpi, av, m.Name, m.JID, 8))
			var parts []string
			if m.Push != "" && m.Push != m.Name {
				parts = append(parts, "~"+m.Push)
			}
			if m.Phone != "" && m.Phone != m.Name {
				parts = append(parts, "+"+m.Phone)
			}
			title, sub = m.Name, strings.Join(parts, " · ")
		}
		x := av.X + av.Width + s(8)
		half := (r.Width - x - s(4)) / 2
		errs = append(errs,
			drawText(c, dpi, title, bodyPt, 0, theme.Color(p.Text), walk.Rectangle{X: x, Y: row.Y, Width: half, Height: row.Height}, leftText),
			drawText(c, dpi, sub, namePt, 0, theme.Color(p.TextSecondary), walk.Rectangle{X: x + half, Y: row.Y, Width: half, Height: row.Height}, leftText))
	}
	return errors.Join(errs...)
}

func (b *InputBar) paintBanner(c *walk.Canvas, _ walk.Rectangle) error {
	p, r, dpi := b.pal, b.banner.ClientBoundsPixels(), b.banner.DPI()
	s := scaler(dpi)
	bg, err := brushFor(p.Sidebar)
	if err != nil {
		return err
	}
	bar, err := brushFor(p.Accent)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, r); err != nil || b.reply == nil {
		return err
	}
	snippet := cmp.Or(strings.Join(strings.Fields(b.reply.Body), " "), quotedKinds[b.reply.Type], "Message")
	line := walk.Rectangle{X: s(quotePadL), Width: r.Width - s(quotePadL) - s(replyCloseW), Height: s(18)}
	second := line
	second.Y = s(18)
	return errors.Join(
		c.FillRectanglePixels(bar, walk.Rectangle{Width: s(3), Height: s(replyBannerH - 6)}),
		drawText(c, dpi, "Replying to "+b.replyAuthor, namePt, walk.FontBold, theme.Color(p.Accent), line, leftText),
		drawText(c, dpi, snippet, namePt, 0, theme.Color(p.TextSecondary), second, leftText),
		drawText(c, dpi, "×", 13, 0, theme.Color(p.TextSecondary), walk.Rectangle{X: r.Width - s(replyCloseW), Width: s(replyCloseW), Height: s(replyBannerH - 6)}, centerText))
}

// Restore puts back text that failed to send, unless the box holds a new draft; no typing signal
// fires.
func (b *InputBar) Restore(text string) { b.composer.restore(text) }

// Focus puts the keyboard focus in the message box.
func (b *InputBar) Focus() { focusOn(b.edit) }

// SetEnabled enables or disables the whole bar.
func (b *InputBar) SetEnabled(enabled bool) {
	b.Composite.SetEnabled(enabled)
	logErr("send repaint", b.sendBtn.Invalidate())
}

// SetPalette recolours the bar.
func (b *InputBar) SetPalette(p theme.Palette) error {
	b.pal = p
	b.edit.SetTextColor(theme.Color(p.Text))
	return errors.Join(setBg(b.Composite, p.Header), setBg(b.field, p.Sidebar), setBg(b.edit, p.Sidebar),
		b.banner.Invalidate(), b.list.Invalidate(), b.emoji.Invalidate(), b.gif.Invalidate(), b.attach.Invalidate(), b.sendBtn.Invalidate())
}

// newButton adds a round button painted with icon; a click, Enter or Space runs act.
func (b *InputBar) newButton(name string, icon icons.Icon, act func()) (*walk.CustomWidget, error) {
	var cw *walk.CustomWidget
	cw, err := newCanvas(b.Composite, func(c *walk.Canvas, _ walk.Rectangle) error { return b.paintButton(c, cw, icon) })
	if err != nil {
		return nil, err
	}
	if err := errors.Join(cw.SetMinMaxSize(walk.Size{Width: barButton, Height: barButton}, walk.Size{Width: barButton, Height: barButton}),
		cw.SetToolTipText(name), focusable(cw, walk.AccRolePushbutton, name, func(k walk.Key) bool {
			if k == walk.KeyReturn || k == walk.KeySpace {
				act()
				return true
			}
			return false
		})); err != nil {
		return nil, err
	}
	cw.MouseDown().Attach(func(_, _ int, btn walk.MouseButton) {
		if btn == walk.LeftButton { // the whole widget is the button: nothing to resolve first
			focusOn(cw)
			act()
		}
	})
	return cw, nil
}

func (b *InputBar) paintButton(c *walk.Canvas, cw *walk.CustomWidget, icon icons.Icon) error {
	p, r := b.pal, cw.ClientBoundsPixels()
	s := scaler(cw.DPI())
	bg, err := brushFor(p.Header)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, r); err != nil {
		return err
	}
	cx, cy := r.Width/2, r.Height/2
	if focusCues(cw) {
		if err := focusRing(c, cx, cy, s(barButton), s(2), theme.Color(p.Text), theme.Color(p.Header)); err != nil {
			return err
		}
	}
	if icon != icons.IconSend {
		return icons.Draw(c, icon, square(cx, cy, s(24)), theme.Color(p.TextSecondary))
	}
	fill, fg := theme.Color(p.Sidebar), theme.Color(p.TextSecondary) // disabled: nothing to send
	if cw.Enabled() {
		fill, fg = theme.Color(p.Accent), walk.RGB(255, 255, 255)
	}
	return errors.Join(icons.Fill(c, square(cx, cy, s(34)), fill), icons.Draw(c, icon, square(cx-s(1), cy+s(1), s(20)), fg))
}

func (b *InputBar) edited() {
	b.composer.edited()
	b.refreshMentions()
	if _, ok := outgoingText(b.edit.Text()); ok != b.canSend {
		b.canSend = ok
		b.sendBtn.SetEnabled(ok)
		logErr("send repaint", b.sendBtn.Invalidate())
	}
	b.fit()
}

// sendNow sends the box's text (Enter or the Send button) and leaves the focus in the box.
func (b *InputBar) sendNow() {
	b.send()
	focusOn(b.edit)
}

// fit sizes the bar for the box's display lines, one to inputMaxLines. While every line fits, the
// box shows them from the first: the edit control scrolled when its text outgrew its old height.
func (b *InputBar) fit() {
	n := int(b.edit.SendMessage(win.EM_GETLINECOUNT, 0, 0))
	lh := 18
	if px := lineHeight(b.edit.Handle()); px > 0 {
		lh = walk.IntTo96DPI(px, b.edit.DPI()) + 1 // rounded up: a clipped last line would scroll
	}
	over := min(len(b.picker.items), mentionRows) * mentionRowH // what sits over the box
	if b.reply != nil {
		over += replyBannerH
	}
	if h := max(barButton, max(1, min(n, inputMaxLines))*lh+2*fieldPadY+over) + 2*barPadY; h != b.height {
		b.height = h
		logErr("input bar height", b.SetMinMaxSize(walk.Size{Height: h}, walk.Size{Height: h}))
	}
	if first := int(b.edit.SendMessage(win.EM_GETFIRSTVISIBLELINE, 0, 0)); n <= inputMaxLines && first > 0 {
		b.edit.SendMessage(win.EM_LINESCROLL, 0, uintptr(-first))
	}
}

// lineHeight is the height in pixels of a line of h's font, 0 when it can't be read.
func lineHeight(h win.HWND) int {
	dc := win.GetDC(h)
	if dc == 0 {
		return 0
	}
	defer win.ReleaseDC(h, dc)
	if f := win.HGDIOBJ(win.SendMessage(h, win.WM_GETFONT, 0, 0)); f != 0 {
		defer win.SelectObject(dc, win.SelectObject(dc, f))
	}
	var tm win.TEXTMETRIC
	if !win.GetTextMetrics(dc, &tm) {
		return 0
	}
	return int(tm.TmHeight)
}

// apiKey is the user's GIPHY API key, "" when none is set.
func (b *InputBar) apiKey() string {
	if b.gifKey == nil {
		return ""
	}
	return strings.TrimSpace(b.gifKey())
}

// pickFile asks for a file to send.
func (b *InputBar) pickFile() {
	dlg := walk.FileDialog{Title: "Attach a file"}
	ok, err := dlg.ShowOpen(b.Form())
	if err != nil {
		logErr("attach", err)
		return
	}
	if ok && b.onAttach != nil {
		b.onAttach(dlg.FilePath)
	}
}

// enterSends reports whether key k sends the message: Enter does, Shift+Enter breaks the line.
func enterSends(k walk.Key, shift bool) bool { return k == walk.KeyReturn && !shift }

// outgoingText is the box's text as sent: trimmed; ok is false when nothing is left.
func outgoingText(s string) (text string, ok bool) {
	s = strings.TrimSpace(s)
	return s, s != ""
}

// textBox is the message box as the composer sees it; *walk.TextEdit is one.
type textBox interface {
	Text() string
	SetText(string) error
}

// composer holds the bar's send and typing rules, apart from its window.
type composer struct {
	box         textBox
	typing      typingSignal
	onSend      func(text string)
	reply       *store.Message // the message the next one replies to; nil for none
	replyAuthor string         // its sender's display name
	picker      mentionPicker
}

// reset empties the box and drops the pending reply and the group's members, for another chat.
func (c *composer) reset() {
	c.typing.stop()
	logErr("clear input", c.box.SetText(""))
	c.clearReply()
	c.picker = mentionPicker{}
}

func (c *composer) setReply(m store.Message, author string) { c.reply, c.replyAuthor = &m, author }

func (c *composer) clearReply() { c.reply, c.replyAuthor = nil, "" }

func (c *composer) replyTarget() (store.Message, bool) {
	if c.reply == nil {
		return store.Message{}, false
	}
	return *c.reply, true
}

// send sends the box's text, trimmed, and empties the box; blank text stays. It reports whether
// anything was sent.
func (c *composer) send() bool {
	text, ok := outgoingText(c.box.Text())
	if !ok {
		return false
	}
	logErr("clear input", c.box.SetText(""))
	c.typing.stop()
	if c.onSend != nil {
		c.onSend(text)
	}
	return true
}

// restore puts text back in the box when it is blank (a new draft wins), without a typing signal,
// and reports whether it did.
func (c *composer) restore(text string) bool {
	if _, typed := outgoingText(c.box.Text()); typed {
		return false
	}
	fire := c.typing.fire
	c.typing.fire = nil // the box reports the change as an edit
	logErr("restore input", c.box.SetText(text))
	c.typing.stop()
	c.typing.fire = fire
	return true
}

// edited follows a change of the box's text: typing goes on, or stops once the box is blank.
func (c *composer) edited() {
	if _, ok := outgoingText(c.box.Text()); ok {
		c.typing.edited()
	} else {
		c.typing.stop()
	}
}

// typingSignal turns edits into typing signals: composing on the first edit after a pause, paused
// once typingIdle passes without one, or on stop. Its one-shot timer runs only while composing.
type typingSignal struct {
	after  func(d time.Duration, f func()) (stop func() bool) // f runs on the UI thread
	fire   func(composing bool)                               // may be nil
	cancel func() bool                                        // stops the idle timer; nil when paused
	gen    int                                                // per timer: an expiry already queued when it was stopped is ignored
}

func (t *typingSignal) edited() {
	if t.cancel != nil {
		t.cancel()
	} else if t.fire != nil {
		t.fire(true)
	}
	t.gen++
	gen := t.gen
	t.cancel = t.after(typingIdle, func() {
		if gen == t.gen {
			t.stop()
		}
	})
}

func (t *typingSignal) stop() {
	if t.cancel == nil {
		return
	}
	t.cancel()
	t.cancel = nil
	t.gen++
	if t.fire != nil {
		t.fire(false)
	}
}

// enterHook routes Enter in a message box to send, after key had its pick of the keys.
type enterHook struct {
	prev uintptr // the window procedure walk installed
	send func()
	key  func(k walk.Key, act bool) bool // takes a key before the box does; act false: only asks
}

var (
	enterHooks   = map[win.HWND]*enterHook{} // UI thread only
	enterHookPtr = syscall.NewCallback(enterHookProc)
)

func enterHookProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	h := enterHooks[hwnd]
	switch msg {
	case win.WM_GETDLGCODE:
		// walk's message loop runs IsDialogMessage, which spends Tab on moving the focus and Esc on
		// Cancel unless the box asks for them: it does only while key has a use for them
		if (wParam == win.VK_TAB || wParam == win.VK_ESCAPE) && h.key(walk.Key(wParam), false) {
			return win.CallWindowProc(h.prev, hwnd, msg, wParam, lParam) | win.DLGC_WANTMESSAGE
		}
	case win.WM_KEYDOWN:
		if h.key(walk.Key(wParam), true) {
			return 0
		}
		if enterSends(walk.Key(wParam), walk.ShiftDown()) {
			h.send()
			return 0
		}
	case win.WM_CHAR: // the line break Enter (or Ctrl+Enter) types: only Shift+Enter keeps it
		if (wParam == '\r' || wParam == '\n') && enterSends(walk.KeyReturn, walk.ShiftDown()) {
			return 0
		}
		if wParam == '\t' || wParam == 0x1b { // a Tab or Esc key took: it types nothing
			return 0
		}
	case win.WM_NCDESTROY:
		delete(enterHooks, hwnd)
	}
	return win.CallWindowProc(h.prev, hwnd, msg, wParam, lParam)
}

// enterSendsIn makes Enter in te call send instead of breaking the line; Shift+Enter still does.
// key sees every key first and reports whether it took it (enterHook.key).
func enterSendsIn(te *walk.TextEdit, send func(), key func(k walk.Key, act bool) bool) error {
	h := te.Handle()
	enterHooks[h] = &enterHook{prev: win.GetWindowLongPtr(h, win.GWLP_WNDPROC), send: send, key: key}
	if win.SetWindowLongPtr(h, win.GWLP_WNDPROC, enterHookPtr) == 0 {
		delete(enterHooks, h)
		return errors.New("ui: subclassing the message box failed")
	}
	return nil
}
