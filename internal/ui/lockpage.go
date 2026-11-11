package ui

import (
	"errors"
	"fmt"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/theme"
)

// LockPage is the full-window page that asks for the PIN. The PIN lives only in its box: it is
// handed to OnUnlock and never kept or logged. UI thread only.
type LockPage struct {
	*walk.Composite
	*kit
	pin      *walk.LineEdit
	status   *label
	unlock   *button
	reset    *button
	logo     *walk.Icon // nil: a glyph stands in
	onUnlock func(pin string)
	onReset  func()
	gen      int // bumps whenever a countdown ends or restarts: older ticks stop
	waiting  bool
}

// NewLockPage creates the page in parent, hidden.
func NewLockPage(parent walk.Container, p theme.Palette) (*LockPage, error) {
	c, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	g := &LockPage{Composite: c, kit: &kit{pal: p}}
	l := walk.NewHBoxLayout()
	if err := errors.Join(setBox(c, l, walk.Margins{}), g.themed(c, false)); err != nil {
		return nil, err
	}
	if _, err := walk.NewHSpacer(c); err != nil {
		return nil, err
	}
	col, err := walk.NewComposite(c)
	if err != nil {
		return nil, err
	}
	cl := walk.NewVBoxLayout()
	if err := errors.Join(setBox(col, cl, walk.Margins{}), cl.SetSpacing(8), g.themed(col, false),
		col.SetMinMaxSize(walk.Size{Width: 360}, walk.Size{Width: 360})); err != nil {
		return nil, err
	}
	if _, err := walk.NewHSpacer(c); err != nil {
		return nil, err
	}
	if _, err := walk.NewVSpacer(col); err != nil {
		return nil, err
	}
	g.logo, err = walk.NewIconFromResourceWithSize("APP", walk.Size{Width: 128, Height: 128})
	logErr("lock page logo", err) // the glyph stands in
	if err := g.newLogo(col, g.logo, true); err != nil {
		return nil, err
	}
	title, err := g.newLabel(col, "Wisp is locked", 20, walk.FontBold, toneText, fieldH)
	if err != nil {
		return nil, err
	}
	hint, err := g.newLabel(col, "Enter your PIN to see your chats", 10, 0, toneSecondary, fieldH)
	if err != nil {
		return nil, err
	}
	title.mid, hint.mid = true, true
	if g.pin, err = pinField(g.kit, col, "PIN", g.submit); err != nil {
		return nil, err
	}
	if g.status, err = g.newLabel(col, "", 10, 0, toneError, 28); err != nil {
		return nil, err
	}
	g.status.mid = true
	row, err := g.newRow(col, fieldH)
	if err != nil {
		return nil, err
	}
	if _, err := walk.NewHSpacer(row); err != nil {
		return nil, err
	}
	if g.unlock, err = g.newButton(row, "Unlock", btnPrimary, 120, g.submit); err != nil {
		return nil, err
	}
	if _, err := walk.NewHSpacer(row); err != nil {
		return nil, err
	}
	forgot, err := g.newRow(col, fieldH)
	if err != nil {
		return nil, err
	}
	if _, err := walk.NewHSpacer(forgot); err != nil {
		return nil, err
	}
	q, err := g.newLabel(forgot, "Forgot your PIN?", 10, 0, toneSecondary, fieldH)
	if err != nil {
		return nil, err
	}
	if err := q.SetMinMaxSize(walk.Size{Width: 120, Height: fieldH}, walk.Size{Width: 120, Height: fieldH}); err != nil {
		return nil, err
	}
	if g.reset, err = g.newButton(forgot, "Reset Wisp", btnLink, 100, func() {
		if g.onReset != nil {
			g.onReset()
		}
	}); err != nil {
		return nil, err
	}
	if _, err := walk.NewHSpacer(forgot); err != nil {
		return nil, err
	}
	if _, err := walk.NewVSpacer(col); err != nil {
		return nil, err
	}
	c.SetVisible(false)
	return g, nil
}

// submit hands the typed PIN to OnUnlock; an empty box does nothing.
func (g *LockPage) submit() {
	if pin := g.pin.Text(); pin != "" && g.onUnlock != nil && !g.waiting {
		g.onUnlock(pin)
	}
}

// OnUnlock sets what Unlock (or Enter in the box) does with the typed PIN.
func (g *LockPage) OnUnlock(fn func(pin string)) { g.onUnlock = fn }

// OnReset sets what the Reset Wisp link does.
func (g *LockPage) OnReset(fn func()) { g.onReset = fn }

// Say shows text under the box, in the error colour; "" clears it.
func (g *LockPage) Say(text string) { g.status.set(text, toneError) }

// Clear empties the box and the message and ends a countdown, enabling the box.
func (g *LockPage) Clear() {
	g.gen++
	g.waiting = false
	g.pin.SetEnabled(true)
	logErr("lock page clear", g.pin.SetText(""))
	g.Say("")
}

// Wait disables the box for d, counting down in "Try again in N s", then enables it again. It ticks
// only while it counts.
func (g *LockPage) Wait(d time.Duration) {
	g.gen++
	gen, end := g.gen, time.Now().Add(d)
	g.waiting = true
	g.pin.SetEnabled(false)
	logErr("lock page clear", g.pin.SetText(""))
	var tick func()
	tick = func() {
		if g.gen != gen || g.IsDisposed() {
			return
		}
		left := time.Until(end)
		if left <= 0 {
			g.Clear()
			g.Focus()
			return
		}
		g.Say(fmt.Sprintf("Try again in %d s", int((left+time.Second-1)/time.Second)))
		time.AfterFunc(min(left, time.Second), func() { g.Synchronize(tick) })
	}
	tick()
}

// Focus puts the keyboard in the PIN box (on the link while the box is disabled), when the window is
// on screen.
func (g *LockPage) Focus() {
	if f := g.Form(); f == nil || !win.IsWindowVisible(g.Handle()) || win.IsIconic(f.Handle()) {
		return
	}
	if g.waiting {
		focusOn(g.reset)
		return
	}
	focusOn(g.pin)
}

// SetPalette recolours the page.
func (g *LockPage) SetPalette(p theme.Palette) error { return g.setPalette(p) }
